package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/drand/drand/metrics"
	"github.com/drand/drand/metrics/pprof"
	"github.com/drand/drand/protobuf/drand"

	"github.com/drand/drand/common"
	"github.com/drand/drand/http"

	"github.com/drand/drand/key"
	"github.com/drand/drand/log"

	"github.com/drand/drand/net"
)

type DrandDaemon struct {
	initialStores   map[string]*key.Store
	beaconProcesses map[string]*BeaconProcess

	privGateway *net.PrivateGateway
	pubGateway  *net.PublicGateway
	control     net.ControlListener

	opts *Config
	log  log.Logger

	// global state lock
	state  sync.Mutex
	exitCh chan bool

	// version indicates the base code variant
	version common.Version
}

// NewDrandDaemon creates a new instance of DrandDaemon
func NewDrandDaemon(c *Config) (*DrandDaemon, error) {
	logger := c.Logger()
	if !c.insecure && (c.certPath == "" || c.keyPath == "") {
		return nil, errors.New("config: need to set WithInsecure if no certificate and private key path given")
	}

	drandDaemon := &DrandDaemon{
		opts:            c,
		log:             logger,
		exitCh:          make(chan bool, 1),
		version:         common.GetAppVersion(),
		initialStores:   make(map[string]*key.Store),
		beaconProcesses: make(map[string]*BeaconProcess),
	}

	if err := drandDaemon.init(); err != nil {
		return nil, err
	}

	return drandDaemon, nil
}

func (dd *DrandDaemon) RemoteStatus(ctx context.Context, request *drand.RemoteStatusRequest) (*drand.RemoteStatusResponse, error) {
	bp, _, err := dd.getBeaconProcess(request.Metadata)
	if err != nil {
		return nil, err
	}

	return bp.RemoteStatus(ctx, request)
}

func (dd *DrandDaemon) init() error {
	c := dd.opts

	// Set the private API address to the command-line flag, if given.
	// Otherwise, set it to the address associated with stored private key.
	privAddr := c.PrivateListenAddress("")
	pubAddr := c.PublicListenAddress("")

	if privAddr == "" {
		return fmt.Errorf("private listen address cannot be empty")
	}

	// ctx is used to create the gateway below.
	// Gateway constructors (specifically, the generated gateway stubs that require it)
	// do not actually use it, so we are passing a background context to be safe.
	ctx := context.Background()

	var err error
	dd.log.Infow("", "network", "init", "insecure", c.insecure)

	if pubAddr != "" {
		handler, err := http.New(ctx, &drandProxy{dd}, c.Version(), dd.log.With("server", "http"))
		if err != nil {
			return err
		}
		if dd.pubGateway, err = net.NewRESTPublicGateway(ctx, pubAddr, c.certPath, c.keyPath, c.certmanager, handler, c.insecure); err != nil {
			return err
		}
	}

	dd.privGateway, err = net.NewGRPCPrivateGateway(ctx, privAddr, c.certPath, c.keyPath, c.certmanager, dd, c.insecure, c.grpcOpts...)
	if err != nil {
		return err
	}

	p := c.ControlPort()
	dd.control = net.NewTCPGrpcControlListener(dd, p)
	go dd.control.Start()

	dd.log.Infow("", "private_listen", privAddr, "control_port", c.ControlPort(), "public_listen", pubAddr, "folder", c.ConfigFolderMB())
	dd.privGateway.StartAll()
	if dd.pubGateway != nil {
		dd.pubGateway.StartAll()
	}

	return nil
}

// InstantiateBeaconProcess creates a new BeaconProcess linked to beacon with id 'beaconID'
func (dd *DrandDaemon) InstantiateBeaconProcess(beaconID string, store key.Store) (*BeaconProcess, error) {
	if beaconID == "" {
		beaconID = common.DefaultBeaconID
	}

	bp, err := NewBeaconProcess(dd.log, store, dd.opts, dd.privGateway, dd.pubGateway)
	if err != nil {
		return nil, err
	}

	dd.state.Lock()
	dd.beaconProcesses[beaconID] = bp
	dd.state.Unlock()

	return bp, nil
}

// LoadBeacons checks for existing stores and creates the corresponding BeaconProcess
// accordingly to each stored BeaconID
func (dd *DrandDaemon) LoadBeacons(metricsFlag string) error {
	// Load possible existing stores
	stores, err := key.NewFileStores(dd.opts.ConfigFolderMB())
	if err != nil {
		return err
	}

	for beaconID, fs := range stores {
		bp, err := dd.InstantiateBeaconProcess(beaconID, fs)
		if err != nil {
			fmt.Printf("beacon id [%s]: can't instantiate randomness beacon. err: %s \n", beaconID, err)
			return err
		}

		freshRun, err := bp.Load()
		if err != nil {
			return err
		}

		if freshRun {
			fmt.Printf("beacon id [%s]: will run as fresh install -> expect to run DKG.\n", beaconID)
		} else {
			fmt.Printf("beacon id [%s]: will start running randomness beacon.\n", beaconID)
			// XXX make it configurable so that new share holder can still start if
			// nobody started.
			// drand.StartBeacon(!c.Bool(pushFlag.Name))
			catchup := true
			bp.StartBeacon(catchup)
		}

		// Start metrics server
		if metricsFlag != "" {
			_ = metrics.Start(metricsFlag, pprof.WithProfile(), bp.PeerMetrics)
		}
	}

	return nil
}
