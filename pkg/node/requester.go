package node

import (
	"context"
	"net/url"

	"github.com/bacalhau-project/bacalhau/pkg/requester/pubsub/jobinfo"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/rs/zerolog/log"

	"github.com/bacalhau-project/bacalhau/pkg/bidstrategy/semantic"
	"github.com/bacalhau-project/bacalhau/pkg/compute"
	"github.com/bacalhau-project/bacalhau/pkg/eventhandler"
	"github.com/bacalhau-project/bacalhau/pkg/jobstore"
	"github.com/bacalhau-project/bacalhau/pkg/model"
	"github.com/bacalhau-project/bacalhau/pkg/publicapi"
	"github.com/bacalhau-project/bacalhau/pkg/requester"
	"github.com/bacalhau-project/bacalhau/pkg/requester/discovery"
	requester_publicapi "github.com/bacalhau-project/bacalhau/pkg/requester/publicapi"
	"github.com/bacalhau-project/bacalhau/pkg/requester/ranking"
	"github.com/bacalhau-project/bacalhau/pkg/requester/retry"
	"github.com/bacalhau-project/bacalhau/pkg/requester/selection"
	"github.com/bacalhau-project/bacalhau/pkg/routing"
	"github.com/bacalhau-project/bacalhau/pkg/storage"
	"github.com/bacalhau-project/bacalhau/pkg/system"
	"github.com/bacalhau-project/bacalhau/pkg/transport/bprotocol"
)

type Requester struct {
	// Visible for testing
	Endpoint           requester.Endpoint
	JobStore           jobstore.Store
	NodeDiscoverer     requester.NodeDiscoverer
	computeProxy       *bprotocol.ComputeProxy
	localCallback      compute.Callback
	requesterAPIServer *requester_publicapi.RequesterAPIServer
	cleanupFunc        func(ctx context.Context)
}

//nolint:funlen
func NewRequesterNode(
	ctx context.Context,
	cleanupManager *system.CleanupManager,
	host host.Host,
	apiServer *publicapi.APIServer,
	config RequesterConfig,
	jobStore jobstore.Store,
	storageProviders storage.StorageProvider,
	jobInfoPublisher *jobinfo.Publisher,
	nodeInfoStore routing.NodeInfoStore,
) (*Requester, error) {
	// prepare event handlers
	tracerContextProvider := eventhandler.NewTracerContextProvider(host.ID().String())
	localJobEventConsumer := eventhandler.NewChainedJobEventHandler(tracerContextProvider)

	// compute proxy
	computeProxy := bprotocol.NewComputeProxy(bprotocol.ComputeProxyParams{
		Host: host,
	})

	// compute node discoverer
	nodeDiscoveryChain := discovery.NewChain(true)
	nodeDiscoveryChain.Add(
		discovery.NewStoreNodeDiscoverer(discovery.StoreNodeDiscovererParams{
			Store: nodeInfoStore,
		}),
		discovery.NewIdentityNodeDiscoverer(discovery.IdentityNodeDiscovererParams{
			Host: host,
		}),
	)

	// compute node ranker
	nodeRankerChain := ranking.NewChain()
	nodeRankerChain.Add(
		// rankers that act as filters and give a -1 score to nodes that do not match the filter
		ranking.NewEnginesNodeRanker(),
		ranking.NewPublishersNodeRanker(),
		ranking.NewStoragesNodeRanker(),
		ranking.NewLabelsNodeRanker(),
		ranking.NewMaxUsageNodeRanker(),
		ranking.NewMinVersionNodeRanker(ranking.MinVersionNodeRankerParams{MinVersion: config.MinBacalhauVersion}),
		ranking.NewPreviousExecutionsNodeRanker(ranking.PreviousExecutionsNodeRankerParams{JobStore: jobStore}),
		// arbitrary rankers
		ranking.NewRandomNodeRanker(ranking.RandomNodeRankerParams{
			RandomnessRange: config.NodeRankRandomnessRange,
		}),
	)

	retryStrategy := config.RetryStrategy
	if retryStrategy == nil {
		// retry strategy
		retryStrategyChain := retry.NewChain()
		retryStrategyChain.Add(
			retry.NewFixedStrategy(retry.FixedStrategyParams{ShouldRetry: true}),
		)
		retryStrategy = retryStrategyChain
	}

	nodeSelector := selection.NewNodeSelectorSwitch(
		selection.NewAnyNodeSelector(selection.AnyNodeSelectorParams{
			NodeDiscoverer:       nodeDiscoveryChain,
			NodeRanker:           nodeRankerChain,
			OverAskForBidsFactor: config.OverAskForBidsFactor,
		}),
		selection.NewAllNodeSelector(selection.AllNodeSelectorParams{
			NodeDiscoverer: nodeDiscoveryChain,
			NodeRanker:     nodeRankerChain,
		}),
	)
	emitter := requester.NewEventEmitter(requester.EventEmitterParams{
		EventConsumer: localJobEventConsumer,
	})
	scheduler := requester.NewBaseScheduler(requester.BaseSchedulerParams{
		ID:               host.ID().String(),
		Host:             host,
		JobStore:         jobStore,
		NodeSelector:     nodeSelector,
		RetryStrategy:    retryStrategy,
		ComputeEndpoint:  computeProxy,
		StorageProviders: storageProviders,
		EventEmitter:     emitter,
		GetVerifyCallback: func() *url.URL {
			return apiServer.GetURI().JoinPath(requester_publicapi.APIPrefix, requester_publicapi.VerifyRoute)
		},
	})
	queue := requester.NewQueue(jobStore, scheduler, emitter)

	publicKey := host.Peerstore().PubKey(host.ID())
	marshaledPublicKey, err := crypto.MarshalPublicKey(publicKey)
	if err != nil {
		return nil, err
	}

	selectionStrategy := semantic.FromJobSelectionPolicy(config.JobSelectionPolicy)

	endpoint := requester.NewBaseEndpoint(&requester.BaseEndpointParams{
		ID:                         host.ID().String(),
		PublicKey:                  marshaledPublicKey,
		Selector:                   selectionStrategy,
		ComputeEndpoint:            computeProxy,
		Store:                      jobStore,
		Queue:                      queue,
		StorageProviders:           storageProviders,
		MinJobExecutionTimeout:     config.MinJobExecutionTimeout,
		DefaultJobExecutionTimeout: config.DefaultJobExecutionTimeout,
		GetBiddingCallback: func() *url.URL {
			return apiServer.GetURI().JoinPath(requester_publicapi.APIPrefix, requester_publicapi.ApprovalRoute)
		},
	})

	housekeeping := requester.NewHousekeeping(requester.HousekeepingParams{
		Endpoint: endpoint,
		JobStore: jobStore,
		NodeID:   host.ID().String(),
		Interval: config.HousekeepingBackgroundTaskInterval,
	})

	// register a handler for the bacalhau protocol handler that will forward requests to the scheduler
	bprotocol.NewCallbackHandler(bprotocol.CallbackHandlerParams{
		Host:     host,
		Callback: scheduler,
	})

	// register debug info providers for the /debug endpoint
	debugInfoProviders := []model.DebugInfoProvider{
		discovery.NewDebugInfoProvider(nodeDiscoveryChain),
	}

	// register requester public http apis
	requesterAPIServer := requester_publicapi.NewRequesterAPIServer(requester_publicapi.RequesterAPIServerParams{
		APIServer:          apiServer,
		Requester:          endpoint,
		DebugInfoProviders: debugInfoProviders,
		JobStore:           jobStore,
		StorageProviders:   storageProviders,
		NodeDiscoverer:     nodeDiscoveryChain,
	})
	err = requesterAPIServer.RegisterAllHandlers()
	if err != nil {
		return nil, err
	}

	// Register event handlers
	lifecycleEventHandler := system.NewJobLifecycleEventHandler(host.ID().String())
	eventTracer, err := eventhandler.NewTracer()
	if err != nil {
		return nil, err
	}

	// order of event handlers is important as triggering some handlers might depend on the state of others.
	localJobEventConsumer.AddHandlers(
		// add tracing metadata to the context about the read event
		eventhandler.JobEventHandlerFunc(lifecycleEventHandler.HandleConsumedJobEvent),
		// ends the span for the job if received a terminal event
		tracerContextProvider,
		// record the event in a log
		eventTracer,
		// dispatches events to listening websockets
		requesterAPIServer,
		// publish job events to the network
		jobInfoPublisher,
	)

	// A single cleanup function to make sure the order of closing dependencies is correct
	cleanupFunc := func(ctx context.Context) {
		// stop the housekeeping background task
		housekeeping.Stop()

		cleanupErr := tracerContextProvider.Shutdown()
		if cleanupErr != nil {
			log.Ctx(ctx).Error().Err(cleanupErr).Msg("failed to shutdown tracer context provider")
		}
		cleanupErr = eventTracer.Shutdown()
		if cleanupErr != nil {
			log.Ctx(ctx).Error().Err(cleanupErr).Msg("failed to shutdown event tracer")
		}
	}

	return &Requester{
		Endpoint:           endpoint,
		localCallback:      scheduler,
		NodeDiscoverer:     nodeDiscoveryChain,
		JobStore:           jobStore,
		computeProxy:       computeProxy,
		cleanupFunc:        cleanupFunc,
		requesterAPIServer: requesterAPIServer,
	}, nil
}

func (r *Requester) RegisterLocalComputeEndpoint(endpoint compute.Endpoint) {
	r.computeProxy.RegisterLocalComputeEndpoint(endpoint)
}

func (r *Requester) cleanup(ctx context.Context) {
	r.cleanupFunc(ctx)
}
