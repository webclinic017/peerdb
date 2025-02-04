package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	//nolint:gosec
	_ "net/http/pprof"

	"github.com/PeerDB-io/peer-flow/activities"
	"github.com/PeerDB-io/peer-flow/connectors/utils"
	"github.com/PeerDB-io/peer-flow/connectors/utils/monitoring"
	"github.com/PeerDB-io/peer-flow/generated/protos"
	"github.com/PeerDB-io/peer-flow/shared"
	peerflow "github.com/PeerDB-io/peer-flow/workflows"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uber-go/tally/v4"
	"github.com/uber-go/tally/v4/prometheus"

	prom "github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"
	sdktally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/worker"
)

type WorkerOptions struct {
	TemporalHostPort string
	EnableProfiling  bool
	EnableMetrics    bool
	EnableMonitoring bool
	ProfilingServer  string
	MetricsServer    string
}

func WorkerMain(opts *WorkerOptions) error {
	if opts.EnableProfiling {
		// Start HTTP profiling server with timeouts
		go func() {
			server := http.Server{
				Addr:         opts.ProfilingServer,
				ReadTimeout:  5 * time.Minute,
				WriteTimeout: 15 * time.Minute,
			}

			log.Infof("starting profiling server on %s", opts.ProfilingServer)

			if err := server.ListenAndServe(); err != nil {
				log.Errorf("unable to start profiling server: %v", err)
			}
		}()
	}

	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGQUIT)
		buf := make([]byte, 1<<20)
		for {
			<-sigs
			stacklen := runtime.Stack(buf, true)
			log.Printf("=== received SIGQUIT ===\n*** goroutine dump...\n%s\n*** end\n", buf[:stacklen])
		}
	}()

	var clientOptions client.Options
	if opts.EnableMetrics {
		clientOptions = client.Options{
			HostPort: opts.TemporalHostPort,
			MetricsHandler: sdktally.NewMetricsHandler(newPrometheusScope(
				prometheus.Configuration{
					ListenAddress: opts.MetricsServer,
					TimerType:     "histogram",
				},
			)),
		}
	} else {
		clientOptions = client.Options{
			HostPort: opts.TemporalHostPort,
		}
	}

	catalogMirrorMonitor := monitoring.NewCatalogMirrorMonitor(nil)
	if opts.EnableMonitoring {
		catalogConnectionString, err := genCatalogConnectionString()
		if err != nil {
			log.Fatal(err)
		}
		catalogConn, err := pgxpool.New(context.Background(), catalogConnectionString)
		if err != nil {
			return fmt.Errorf("unable to establish connection with catalog: %w", err)
		}
		catalogMirrorMonitor = monitoring.NewCatalogMirrorMonitor(catalogConn)
	}
	defer catalogMirrorMonitor.Close()

	c, err := client.Dial(clientOptions)
	if err != nil {
		return fmt.Errorf("unable to create Temporal client: %w", err)
	}
	defer c.Close()

	w := worker.New(c, shared.PeerFlowTaskQueue, worker.Options{})
	w.RegisterWorkflow(peerflow.PeerFlowWorkflow)
	w.RegisterWorkflow(peerflow.PeerFlowWorkflowWithConfig)
	w.RegisterWorkflow(peerflow.SyncFlowWorkflow)
	w.RegisterWorkflow(peerflow.SetupFlowWorkflow)
	w.RegisterWorkflow(peerflow.NormalizeFlowWorkflow)
	w.RegisterWorkflow(peerflow.QRepFlowWorkflow)
	w.RegisterWorkflow(peerflow.QRepPartitionWorkflow)
	w.RegisterWorkflow(peerflow.DropFlowWorkflow)
	w.RegisterActivity(&activities.FetchConfigActivity{})
	w.RegisterActivity(&activities.FlowableActivity{
		EnableMetrics:        opts.EnableMetrics,
		CatalogMirrorMonitor: &catalogMirrorMonitor,
	})

	err = w.Run(worker.InterruptCh())
	if err != nil {
		return fmt.Errorf("worker run error: %w", err)
	}

	return nil
}

func newPrometheusScope(c prometheus.Configuration) tally.Scope {
	reporter, err := c.NewReporter(
		prometheus.ConfigurationOptions{
			Registry: prom.NewRegistry(),
			OnError: func(err error) {
				log.Println("error in prometheus reporter", err)
			},
		},
	)
	if err != nil {
		log.Fatalln("error creating prometheus reporter", err)
	}
	scopeOpts := tally.ScopeOptions{
		CachedReporter:  reporter,
		Separator:       prometheus.DefaultSeparator,
		SanitizeOptions: &sdktally.PrometheusSanitizeOptions,
		Prefix:          "flow_worker",
	}
	scope, _ := tally.NewRootScope(scopeOpts, time.Second)
	scope = sdktally.NewPrometheusNamingScope(scope)

	log.Println("prometheus metrics scope created")
	return scope
}

func genCatalogConnectionString() (string, error) {
	host, ok := os.LookupEnv("PEERDB_CATALOG_HOST")
	if !ok {
		return "", fmt.Errorf("PEERDB_CATALOG_HOST is not set")
	}
	portStr, ok := os.LookupEnv("PEERDB_CATALOG_PORT")
	if !ok {
		return "", fmt.Errorf("PEERDB_CATALOG_PORT is not set")
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return "", fmt.Errorf("unable to parse PEERDB_CATALOG_PORT as unsigned integer")
	}
	user, ok := os.LookupEnv("PEERDB_CATALOG_USER")
	if !ok {
		return "", fmt.Errorf("PEERDB_CATALOG_USER is not set")
	}
	password, ok := os.LookupEnv("PEERDB_CATALOG_PASSWORD")
	if !ok {
		return "", fmt.Errorf("PEERDB_CATALOG_PASSWORD is not set")
	}
	database, ok := os.LookupEnv("PEERDB_CATALOG_DATABASE")
	if !ok {
		return "", fmt.Errorf("PEERDB_CATALOG_DATABASE is not set")
	}

	return utils.GetPGConnectionString(&protos.PostgresConfig{
		Host:     host,
		Port:     uint32(port),
		User:     user,
		Password: password,
		Database: database,
	}), nil
}
