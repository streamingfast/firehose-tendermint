package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/streamingfast/bstream"
	"github.com/streamingfast/bstream/blockstream"
	"github.com/streamingfast/dgrpc"
	"github.com/streamingfast/dlauncher/launcher"
	"github.com/streamingfast/logging"
	"github.com/streamingfast/node-manager/mindreader"

	pbbstream "github.com/streamingfast/pbgo/sf/bstream/v1"
	pbheadinfo "github.com/streamingfast/pbgo/sf/headinfo/v1"
	"github.com/streamingfast/shutter"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/figment-networks/firehose-cosmos/codec"
)

const (
	defaultLineBufferSize = 10 * 1024 * 1024

	modeLogs  = "logs"  // Consume events from the log file(s)
	modeStdin = "stdin" // Consume events from the STDOUT of another process
	modeNode  = "node"  // Consume events from the spawned node process
)

var ingestorLogger, ingestorTracer = logging.PackageLogger("ingestor", "github.com/figment-network/firehose-cosmos/noderunner")

func init() {
	appLogger := ingestorLogger
	appTracer := ingestorTracer

	registerFlags := func(cmd *cobra.Command) error {
		flags := cmd.Flags()

		flags.String("ingestor-mode", modeStdin, "Mode of operation, one of (stdin, logs, node)")
		flags.String("ingestor-logs-dir", "", "Event logs source directory")
		flags.String("ingestor-logs-pattern", "\\.log(\\.[\\d]+)?", "Logs file pattern")
		flags.Int("ingestor-line-buffer-size", defaultLineBufferSize, "Buffer size in bytes for the line reader")
		flags.String("ingestor-working-dir", "{fh-data-dir}/workdir", "Path where mindreader will stores its files")
		flags.String("ingestor-grpc-listen-addr", BlockStreamServingAddr, "GRPC server listen address")
		flags.String("ingestor-node-path", "", "Path to node binary")
		flags.String("ingestor-node-dir", "", "Node working directory")
		flags.String("ingestor-node-args", "", "Node process arguments")
		flags.String("ingestor-node-env", "", "Node process env vars")
		flags.String("ingestor-node-logs-filter", "", "Node process log filter expression")

		return nil
	}

	initFunc := func(runtime *launcher.Runtime) (err error) {
		mode := viper.GetString("ingestor-mode")

		switch mode {
		case modeStdin:
			return nil
		case modeNode:
			return checkNodeBinPath(viper.GetString("ingestor-node-path"))
		case modeLogs:
			return checkLogsSource(viper.GetString("ingestor-logs-dir"))
		default:
			return fmt.Errorf("invalid mode: %v", mode)
		}
	}

	factoryFunc := func(runtime *launcher.Runtime) (launcher.App, error) {
		sfDataDir := runtime.AbsDataDir

		_, oneBlockStoreURL, _, err := GetCommonStoresURLs(runtime.AbsDataDir)
		workingDir := MustReplaceDataDir(sfDataDir, viper.GetString("ingestor-working-dir"))
		gprcListenAdrr := viper.GetString("ingestor-grpc-listen-addr")
		batchStartBlockNum := viper.GetUint64("ingestor-start-block-num")
		batchStopBlockNum := viper.GetUint64("ingestor-stop-block-num")
		oneBlockFileSuffix := viper.GetString("ingestor-oneblock-suffix")
		blocksChanCapacity := viper.GetInt("ingestor-blocks-chan-capacity")

		consoleReaderFactory := func(lines chan string) (mindreader.ConsolerReader, error) {
			return codec.NewConsoleReader(lines, zlog)
		}

		blockStreamServer := blockstream.NewUnmanagedServer(blockstream.ServerOptionWithLogger(appLogger))
		healthCheck := func(ctx context.Context) (isReady bool, out interface{}, err error) {
			return blockStreamServer.Ready(), nil, nil
		}

		server := dgrpc.NewServer2(
			dgrpc.WithLogger(appLogger),
			dgrpc.WithHealthCheck(dgrpc.HealthCheckOverGRPC|dgrpc.HealthCheckOverHTTP, healthCheck),
		)
		server.RegisterService(func(gs *grpc.Server) {
			pbheadinfo.RegisterHeadInfoServer(gs, blockStreamServer)
			pbbstream.RegisterBlockStreamServer(gs, blockStreamServer)
		})

		mrp, err := mindreader.NewMindReaderPlugin(
			oneBlockStoreURL,
			workingDir,
			consoleReaderFactory,
			batchStartBlockNum,
			batchStopBlockNum,
			blocksChanCapacity,
			headBlockUpdater,
			func(error) {},
			oneBlockFileSuffix,
			blockStreamServer,
			appLogger,
			appTracer,
		)
		if err != nil {
			log.Fatal("error initialising mind reader", zap.Error(err))
			return nil, nil
		}

		return &IngestorApp{
			Shutter:          shutter.New(),
			mrp:              mrp,
			mode:             viper.GetString("ingestor-mode"),
			lineBufferSize:   viper.GetInt("ingestor-line-buffer-size"),
			nodeBinPath:      viper.GetString("ingestor-node-path"),
			nodeDir:          viper.GetString("ingestor-node-dir"),
			nodeArgs:         viper.GetString("ingestor-node-args"),
			nodeEnv:          viper.GetString("ingestor-node-env"),
			nodeLogsFilter:   viper.GetString("ingestor-node-logs-filter"),
			logsDir:          viper.GetString("ingestor-logs-dir"),
			logsFilePattern:  viper.GetString("ingestor-logs-pattern"),
			server:           server,
			serverListenAddr: gprcListenAdrr,
		}, nil
	}

	launcher.RegisterApp(&launcher.AppDef{
		ID:            "ingestor",
		Title:         "Ingestor",
		Description:   "Reads the log files produces by the instrumented node",
		MetricsID:     "ingestor",
		Logger:        launcher.NewLoggingDef("ingestor.*", nil),
		RegisterFlags: registerFlags,
		InitFunc:      initFunc,
		FactoryFunc:   factoryFunc,
	})
}

func headBlockUpdater(_ *bstream.Block) error {
	// TODO: will need to be implemented somewhere
	return nil
}

func checkLogsSource(dir string) error {
	if dir == "" {
		return errors.New("ingestor logs dir must be set")
	}

	dir, err := expandDir(dir)
	if err != nil {
		return err
	}

	if !dirExists(dir) {
		return errors.New("ingestor logs dir must exist")
	}

	return nil
}

func checkNodeBinPath(binPath string) error {
	if binPath == "" {
		return errors.New("node path must be set")
	}

	stat, err := os.Stat(binPath)
	if err != nil {
		return fmt.Errorf("cant inspect node path: %w", err)
	}

	if stat.IsDir() {
		return fmt.Errorf("path %v is a directory", binPath)
	}

	return nil
}
