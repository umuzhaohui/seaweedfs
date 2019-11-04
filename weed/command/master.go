package command

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/chrislusf/raft/protobuf"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/server"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/gorilla/mux"
	"github.com/spf13/viper"
	"google.golang.org/grpc/reflection"
)

var (
	m MasterOptions
)

type MasterOptions struct {
	port               *int
	ip                 *string
	ipBind             *string
	metaFolder         *string
	peers              *string
	volumeSizeLimitMB  *uint
	volumePreallocate  *bool
	pulseSeconds       *int
	defaultReplication *string
	garbageThreshold   *float64
	whiteList          *string
	disableHttp        *bool
	metricsAddress     *string
	metricsIntervalSec *int

	sequencerType *string
	etcdUrls      *string
}

func init() {
	cmdMaster.Run = runMaster // break init cycle
	m.port = cmdMaster.Flag.Int("port", 9333, "http listen port")
	m.ip = cmdMaster.Flag.String("ip", "localhost", "master <ip>|<server> address")
	m.ipBind = cmdMaster.Flag.String("ip.bind", "0.0.0.0", "ip address to bind to")
	m.metaFolder = cmdMaster.Flag.String("mdir", os.TempDir(), "data directory to store meta data")
	m.peers = cmdMaster.Flag.String("peers", "", "all master nodes in comma separated ip:port list, example: 127.0.0.1:9093,127.0.0.1:9094")
	m.volumeSizeLimitMB = cmdMaster.Flag.Uint("volumeSizeLimitMB", 30*1000, "Master stops directing writes to oversized volumes.")
	m.volumePreallocate = cmdMaster.Flag.Bool("volumePreallocate", false, "Preallocate disk space for volumes.")
	m.pulseSeconds = cmdMaster.Flag.Int("pulseSeconds", 5, "number of seconds between heartbeats")
	m.defaultReplication = cmdMaster.Flag.String("defaultReplication", "000", "Default replication type if not specified.")
	m.garbageThreshold = cmdMaster.Flag.Float64("garbageThreshold", 0.3, "threshold to vacuum and reclaim spaces")
	m.whiteList = cmdMaster.Flag.String("whiteList", "", "comma separated Ip addresses having write permission. No limit if empty.")
	m.disableHttp = cmdMaster.Flag.Bool("disableHttp", false, "disable http requests, only gRPC operations are allowed.")
	m.metricsAddress = cmdMaster.Flag.String("metrics.address", "", "Prometheus gateway address")
	m.metricsIntervalSec = cmdMaster.Flag.Int("metrics.intervalSeconds", 15, "Prometheus push interval in seconds")
	m.sequencerType = cmdMaster.Flag.String("sequencerType", "memory", "Choose [memory|etcd] type for store the file sequence")
	m.etcdUrls = cmdMaster.Flag.String("etcdUrls", "",
		"when sequencerType=etcd, set etcdUrls for etcd cluster that store file sequence, example : http://127.0.0.1:2379,http://127.0.0.1:2389")
}

var cmdMaster = &Command{
	UsageLine: "master -port=9333",
	Short:     "start a master server",
	Long: `start a master server to provide volume=>location mapping service and sequence number of file ids

	The configuration file "security.toml" is read from ".", "$HOME/.seaweedfs/", or "/etc/seaweedfs/", in that order.

	The example security.toml configuration file can be generated by "weed scaffold -config=security"

  `,
}

var (
	masterCpuProfile = cmdMaster.Flag.String("cpuprofile", "", "cpu profile output file")
	masterMemProfile = cmdMaster.Flag.String("memprofile", "", "memory profile output file")
)

func runMaster(cmd *Command, args []string) bool {

	util.LoadConfiguration("security", false)
	util.LoadConfiguration("master", false)

	runtime.GOMAXPROCS(runtime.NumCPU())
	util.SetupProfiling(*masterCpuProfile, *masterMemProfile)

	if err := util.TestFolderWritable(*m.metaFolder); err != nil {
		glog.Fatalf("Check Meta Folder (-mdir) Writable %s : %s", *m.metaFolder, err)
	}

	var masterWhiteList []string
	if *m.whiteList != "" {
		masterWhiteList = strings.Split(*m.whiteList, ",")
	}
	if *m.volumeSizeLimitMB > util.VolumeSizeLimitGB*1000 {
		glog.Fatalf("volumeSizeLimitMB should be smaller than 30000")
	}

	startMaster(m, masterWhiteList)

	return true
}

func startMaster(masterOption MasterOptions, masterWhiteList []string) {

	myMasterAddress, peers := checkPeers(*masterOption.ip, *masterOption.port, *masterOption.peers)

	r := mux.NewRouter()
	ms := weed_server.NewMasterServer(r, masterOption.toMasterOption(masterWhiteList), peers)
	listeningAddress := *masterOption.ipBind + ":" + strconv.Itoa(*masterOption.port)
	glog.V(0).Infof("Start Seaweed Master %s at %s", util.VERSION, listeningAddress)
	masterListener, e := util.NewListener(listeningAddress, 0)
	if e != nil {
		glog.Fatalf("Master startup error: %v", e)
	}
	// start raftServer
	raftServer := weed_server.NewRaftServer(security.LoadClientTLS(viper.Sub("grpc"), "master"),
		peers, myMasterAddress, *masterOption.metaFolder, ms.Topo, *masterOption.pulseSeconds)
	if raftServer == nil {
		glog.Fatalf("please verify %s is writable, see https://github.com/chrislusf/seaweedfs/issues/717", *masterOption.metaFolder)
	}
	ms.SetRaftServer(raftServer)
	r.HandleFunc("/cluster/status", raftServer.StatusHandler).Methods("GET")
	// starting grpc server
	grpcPort := *masterOption.port + 10000
	grpcL, err := util.NewListener(*masterOption.ipBind+":"+strconv.Itoa(grpcPort), 0)
	if err != nil {
		glog.Fatalf("master failed to listen on grpc port %d: %v", grpcPort, err)
	}
	// Create your protocol servers.
	grpcS := util.NewGrpcServer(security.LoadServerTLS(viper.Sub("grpc"), "master"))
	master_pb.RegisterSeaweedServer(grpcS, ms)
	protobuf.RegisterRaftServer(grpcS, raftServer)
	reflection.Register(grpcS)
	glog.V(0).Infof("Start Seaweed Master %s grpc server at %s:%d", util.VERSION, *masterOption.ipBind, grpcPort)
	go grpcS.Serve(grpcL)

	go ms.MasterClient.KeepConnectedToMaster()

	// start http server
	httpS := &http.Server{Handler: r}
	go httpS.Serve(masterListener)

	select {}
}

func checkPeers(masterIp string, masterPort int, peers string) (masterAddress string, cleanedPeers []string) {
	masterAddress = masterIp + ":" + strconv.Itoa(masterPort)
	if peers != "" {
		cleanedPeers = strings.Split(peers, ",")
	}

	hasSelf := false
	for _, peer := range cleanedPeers {
		if peer == masterAddress {
			hasSelf = true
			break
		}
	}

	if !hasSelf {
		cleanedPeers = append(cleanedPeers, masterAddress)
	}
	if len(cleanedPeers)%2 == 0 {
		glog.Fatalf("Only odd number of masters are supported!")
	}
	return
}

func (m *MasterOptions) toMasterOption(whiteList []string) *weed_server.MasterOption {
	return &weed_server.MasterOption{
		Port:                    *m.port,
		MetaFolder:              *m.metaFolder,
		VolumeSizeLimitMB:       *m.volumeSizeLimitMB,
		VolumePreallocate:       *m.volumePreallocate,
		PulseSeconds:            *m.pulseSeconds,
		DefaultReplicaPlacement: *m.defaultReplication,
		GarbageThreshold:        *m.garbageThreshold,
		WhiteList:               whiteList,
		DisableHttp:             *m.disableHttp,
		MetricsAddress:          *m.metricsAddress,
		MetricsIntervalSec:      *m.metricsIntervalSec,
	}
}
