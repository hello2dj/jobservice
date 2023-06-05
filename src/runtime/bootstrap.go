// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gomodule/redigo/redis"

	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/lib/metric"
	"github.com/hello2dj/jobservice/src/api"
	"github.com/hello2dj/jobservice/src/common/utils"
	"github.com/hello2dj/jobservice/src/config"
	"github.com/hello2dj/jobservice/src/core"
	"github.com/hello2dj/jobservice/src/env"
	"github.com/hello2dj/jobservice/src/hook"
	"github.com/hello2dj/jobservice/src/job"
	"github.com/hello2dj/jobservice/src/job/impl"
	"github.com/hello2dj/jobservice/src/job/impl/sample"
	"github.com/hello2dj/jobservice/src/lcm"
	"github.com/hello2dj/jobservice/src/logger"
	"github.com/hello2dj/jobservice/src/mgt"
	"github.com/hello2dj/jobservice/src/migration"
	redislib "github.com/hello2dj/jobservice/src/redis"
	"github.com/hello2dj/jobservice/src/worker"
	"github.com/hello2dj/jobservice/src/worker/cworker"
)

const (
	dialConnectionTimeout = 30 * time.Second
	dialReadTimeout       = 10 * time.Second
	dialWriteTimeout      = 10 * time.Second
)

// JobService ...
var JobService = &Bootstrap{
	syncEnabled: true,
}

// workerPoolID
var workerPoolID string

// Bootstrap is coordinating process to help load and start the other components to serve.
type Bootstrap struct {
	jobContextInitializer job.ContextInitializer
	syncEnabled           bool
}

// SetJobContextInitializer set the job context initializer
func (bs *Bootstrap) SetJobContextInitializer(initializer job.ContextInitializer) {
	if initializer != nil {
		bs.jobContextInitializer = initializer
	}
}

// LoadAndRun will load configurations, initialize components and then start the related process to serve requests.
// Return error if meet any problems.
func (bs *Bootstrap) LoadAndRun(ctx context.Context, cancel context.CancelFunc) (err error) {
	rootContext := &env.Context{
		SystemContext: ctx,
		WG:            &sync.WaitGroup{},
		ErrorChan:     make(chan error, 5), // with 5 buffers
	}

	// Build specified job context
	if bs.jobContextInitializer != nil {
		rootContext.JobContext, err = bs.jobContextInitializer(ctx)
		if err != nil {
			return errors.Errorf("initialize job context error: %s", err)
		}
	}
	// Make sure the job context is created
	if rootContext.JobContext == nil {
		rootContext.JobContext = impl.NewDefaultContext(ctx)
	}

	// Alliance to config
	cfg := config.DefaultConfig

	ledis, err := redislib.NewLedisServer(cfg.PoolConfig.RedisPoolCfg.RedisURL)
	if err != nil {
		return err
	}

	go ledis.Run()

	var (
		backendWorker worker.Interface
		manager       mgt.Manager
		// syncWorker    *sync2.Worker
	)
	if cfg.PoolConfig.Backend == config.JobServicePoolBackendRedis {
		// Number of workers
		workerNum := cfg.PoolConfig.WorkerCount
		// Add {} to namespace to void slot issue
		namespace := fmt.Sprintf("{%s}", cfg.PoolConfig.RedisPoolCfg.Namespace)
		// Get redis connection pool
		redisPool := bs.getRedisPool(cfg.PoolConfig.RedisPoolCfg)

		// Do data migration if necessary
		rdbMigrator := migration.New(redisPool, namespace)
		rdbMigrator.Register(migration.PolicyMigratorFactory)
		if err := rdbMigrator.Migrate(); err != nil {
			// Just logged, should not block the starting process
			logger.Error(err)
		}

		// Create stats manager
		manager = mgt.NewManager(ctx, namespace, redisPool)
		// Create hook agent, it's a singleton object
		// the retryConcurrency keep same with worker num
		hookAgent := hook.NewAgent(rootContext, namespace, redisPool, workerNum)
		hookCallback := func(URL string, change *job.StatusChange) error {
			msg := fmt.Sprintf(
				"status change: job=%s, status=%s, revision=%d",
				change.JobID,
				change.Status,
				change.Metadata.Revision,
			)
			if !utils.IsEmptyStr(change.CheckIn) {
				// Ignore the real check in message to avoid too big message stream
				cData := change.CheckIn
				if len(cData) > 256 {
					cData = fmt.Sprintf("<DATA BLOCK: %d bytes>", len(cData))
				}
				msg = fmt.Sprintf("%s, check_in=%s", msg, cData)
			}

			evt := &hook.Event{
				URL:       URL,
				Timestamp: change.Metadata.UpdateTime, // use update timestamp to avoid duplicated resending.
				Data:      change,
				Message:   msg,
			}

			// Hook event sending should not influence the main job flow (because job may call checkin() in the job run).
			if err := hookAgent.Trigger(evt); err != nil {
				logger.Error(err)
			}

			return nil
		}

		// Create job life cycle management controller
		lcmCtl := lcm.NewController(rootContext, namespace, redisPool, hookCallback)

		// Start the backend worker
		backendWorker, err = bs.loadAndRunRedisWorkerPool(
			rootContext,
			namespace,
			workerNum,
			redisPool,
			lcmCtl,
		)
		if err != nil {
			return errors.Errorf("load and run worker error: %s", err)
		}

		// Run daemon process of life cycle controller
		// Ignore returned error
		if err = lcmCtl.Serve(); err != nil {
			return errors.Errorf("start life cycle controller error: %s", err)
		}

		// // Initialize sync worker
		// if bs.syncEnabled {
		// 	syncWorker = sync2.New(3).
		// 		WithContext(rootContext).
		// 		UseManager(manager).
		// 		UseScheduler(period.NewScheduler(rootContext.SystemContext, namespace, redisPool, lcmCtl)).
		// 		WithCoreInternalAddr(strings.TrimSuffix(config.GetCoreURL(), "/")).
		// 		UseCoreScheduler(scheduler.Sched).
		// 		UseCoreExecutionManager(task.ExecMgr).
		// 		UseCoreTaskManager(task.Mgr).
		// 		UseQueueStatusManager(queuestatus.Mgr).
		// 		UseMonitorRedisClient(cfg.PoolConfig.RedisPoolCfg).
		// 		WithPolicyLoader(func() ([]*period.Policy, error) {
		// 			conn := redisPool.Get()
		// 			defer conn.Close()

		// 			return period.Load(namespace, conn)
		// 		})
		// 	// Start sync worker
		// 	// Not block the regular process.
		// 	if err := syncWorker.Start(); err != nil {
		// 		logger.Error(err)
		// 	}
		// }
	} else {
		return errors.Errorf("worker backend '%s' is not supported", cfg.PoolConfig.Backend)
	}

	// Initialize controller
	ctl := core.NewController(backendWorker, manager)
	// Initialize Prometheus backend
	// go bs.createMetricServer(cfg)
	// Start the API server
	apiServer := bs.createAPIServer(ctx, cfg, ctl)

	// Listen to the system signals
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	terminated := false
	go func(errChan chan error) {
		defer func() {
			// Gracefully shutdown
			// Error happened here should not override the outside error
			if er := apiServer.Stop(); er != nil {
				logger.Error(er)
			}

			ledis.Close()

			// Notify others who're listening to the system context
			cancel()
		}()

		select {
		case <-sig:
			terminated = true
			return
		case err = <-errChan:
			logger.Errorf("Received error from error chan: %s", err)
			return
		}
	}(rootContext.ErrorChan)

	node := ctx.Value(utils.NodeID)
	// Blocking here
	logger.Infof("API server is serving at %d with [%s] mode at node [%s]", cfg.Port, cfg.Protocol, node)
	metric.JobserviceInfo.WithLabelValues(node.(string), workerPoolID, fmt.Sprint(cfg.PoolConfig.WorkerCount)).Set(1)
	if er := apiServer.Start(); er != nil {
		if !terminated {
			// Tell the listening goroutine
			rootContext.ErrorChan <- er
		}
	} else {
		// In case
		sig <- os.Interrupt
	}

	// Wait everyone exits.
	rootContext.WG.Wait()

	return
}

func (bs *Bootstrap) createMetricServer(cfg *config.Configuration) {
	if cfg.Metric != nil && cfg.Metric.Enabled {
		metric.RegisterJobServiceCollectors()
		metric.ServeProm(cfg.Metric.Path, cfg.Metric.Port)
		logger.Infof("Prom backend is serving at %s:%d", cfg.Metric.Path, cfg.Metric.Port)
	}
}

// Load and run the API server.
func (bs *Bootstrap) createAPIServer(ctx context.Context, cfg *config.Configuration, ctl core.Interface) *api.Server {
	// Initialized API server
	authProvider := &api.SecretAuthenticator{}
	handler := api.NewDefaultHandler(ctl)
	router := api.NewBaseRouter(handler, authProvider)
	serverConfig := api.ServerConfig{
		Protocol: cfg.Protocol,
		Port:     cfg.Port,
	}
	if cfg.HTTPSConfig != nil {
		serverConfig.Protocol = config.JobServiceProtocolHTTPS
		serverConfig.Cert = cfg.HTTPSConfig.Cert
		serverConfig.Key = cfg.HTTPSConfig.Key
	}

	return api.NewServer(ctx, router, serverConfig)
}

// Load and run the worker worker
func (bs *Bootstrap) loadAndRunRedisWorkerPool(
	ctx *env.Context,
	ns string,
	workers uint,
	redisPool *redis.Pool,
	lcmCtl lcm.Controller,
) (worker.Interface, error) {
	redisWorker := cworker.NewWorker(ctx, ns, workers, redisPool, lcmCtl)
	workerPoolID = redisWorker.GetPoolID()

	// Register jobs here
	if err := redisWorker.RegisterJobs(
		map[string]interface{}{
			// Only for debugging and testing purpose
			job.SampleJob: (*sample.Job)(nil),
			// Functional jobs
		}); err != nil {
		// exit
		return nil, err
	}
	if err := redisWorker.Start(); err != nil {
		return nil, err
	}
	return redisWorker, nil
}

// Get a redis connection pool
func (bs *Bootstrap) getRedisPool(redisPoolConfig *config.RedisPoolConfig) *redis.Pool {
	if pool, err := redislib.GetRedisPool("JobService", redisPoolConfig.RedisURL, &redislib.PoolParam{
		PoolMaxIdle:           6,
		PoolIdleTimeout:       time.Duration(redisPoolConfig.IdleTimeoutSecond) * time.Second,
		DialConnectionTimeout: dialConnectionTimeout,
		DialReadTimeout:       dialReadTimeout,
		DialWriteTimeout:      dialWriteTimeout,
	}); err != nil {
		panic(err)
	} else {
		return pool
	}
}
