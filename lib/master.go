package lib

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pingcap/errors"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/pkg/workerpool"
	"go.uber.org/atomic"
	"go.uber.org/dig"
	"go.uber.org/zap"

	"github.com/hanfei1991/microcosm/client"
	runtime "github.com/hanfei1991/microcosm/executor/worker"
	"github.com/hanfei1991/microcosm/lib/config"
	"github.com/hanfei1991/microcosm/lib/master"
	"github.com/hanfei1991/microcosm/lib/metadata"
	libModel "github.com/hanfei1991/microcosm/lib/model"
	"github.com/hanfei1991/microcosm/lib/statusutil"
	"github.com/hanfei1991/microcosm/model"
	"github.com/hanfei1991/microcosm/pb"
	"github.com/hanfei1991/microcosm/pkg/clock"
	dcontext "github.com/hanfei1991/microcosm/pkg/context"
	"github.com/hanfei1991/microcosm/pkg/deps"
	derror "github.com/hanfei1991/microcosm/pkg/errors"
	extKV "github.com/hanfei1991/microcosm/pkg/meta/extension"
	"github.com/hanfei1991/microcosm/pkg/meta/kvclient"
	"github.com/hanfei1991/microcosm/pkg/meta/metaclient"
	"github.com/hanfei1991/microcosm/pkg/p2p"
	"github.com/hanfei1991/microcosm/pkg/quota"
	"github.com/hanfei1991/microcosm/pkg/tenant"
	"github.com/hanfei1991/microcosm/pkg/uuid"
)

type Master interface {
	Init(ctx context.Context) error
	Poll(ctx context.Context) error
	MasterID() libModel.MasterID

	runtime.Closer
}

type MasterImpl interface {
	// InitImpl provides customized logic for the business logic to initialize.
	InitImpl(ctx context.Context) error

	// Tick is called on a fixed interval.
	Tick(ctx context.Context) error

	// OnMasterRecovered is called when the master has recovered from an error.
	OnMasterRecovered(ctx context.Context) error

	// OnWorkerDispatched is called when a request to launch a worker is finished.
	OnWorkerDispatched(worker WorkerHandle, result error) error

	// OnWorkerOnline is called when the first heartbeat for a worker is received.
	OnWorkerOnline(worker WorkerHandle) error

	// OnWorkerOffline is called when a worker exits or has timed out.
	// Worker exit scenario contains normal finish and manually stop
	OnWorkerOffline(worker WorkerHandle, reason error) error

	// OnWorkerMessage is called when a customized message is received.
	OnWorkerMessage(worker WorkerHandle, topic p2p.Topic, message interface{}) error

	// OnWorkerStatusUpdated is called when a worker's status is updated.
	OnWorkerStatusUpdated(worker WorkerHandle, newStatus *libModel.WorkerStatus) error

	// CloseImpl is called when the master is being closed
	CloseImpl(ctx context.Context) error
}

const (
	createWorkerWaitQuotaTimeout = 5 * time.Second
	createWorkerTimeout          = 10 * time.Second
	maxCreateWorkerConcurrency   = 100
)

type BaseMaster interface {
	// MetaKVClient return user metastore kv client
	MetaKVClient() metaclient.KVClient
	Init(ctx context.Context) error
	Poll(ctx context.Context) error
	MasterMeta() *libModel.MasterMetaKVData
	MasterID() libModel.MasterID
	GetWorkers() map[libModel.WorkerID]WorkerHandle
	IsMasterReady() bool
	Close(ctx context.Context) error
	OnError(err error)
	// CreateWorker registers worker handler and dispatches worker to executor
	CreateWorker(workerType WorkerType, config WorkerConfig, cost model.RescUnit) (libModel.WorkerID, error)
}

type DefaultBaseMaster struct {
	Impl MasterImpl

	// dependencies
	messageHandlerManager p2p.MessageHandlerManager
	messageSender         p2p.MessageSender
	// framework metastore prefix kvclient
	metaKVClient metaclient.KVClient
	// user metastore raw kvclient
	userRawKVClient       extKV.KVClientEx
	executorClientManager client.ClientsManager
	serverMasterClient    client.MasterClient
	pool                  workerpool.AsyncPool

	clock clock.Clock

	// workerManager maintains the list of all workers and
	// their statuses.
	workerManager *master.WorkerManager

	currentEpoch atomic.Int64

	wg    sync.WaitGroup
	errCh chan error

	// closeCh is closed when the BaseMaster is exiting
	closeCh chan struct{}

	id            libModel.MasterID // id of this master itself
	advertiseAddr string
	nodeID        p2p.NodeID
	timeoutConfig config.TimeoutConfig
	masterMeta    *libModel.MasterMetaKVData

	// user metastore prefix kvclient
	// Don't close it. It's just a prefix wrapper for underlying userRawKVClient
	userMetaKVClient metaclient.KVClient

	// components for easier unit testing
	uuidGen uuid.Generator

	// TODO use a shared quota for all masters.
	createWorkerQuota quota.ConcurrencyQuota

	// deps is a container for injected dependencies
	deps *deps.Deps
}

type masterParams struct {
	dig.In

	MessageHandlerManager p2p.MessageHandlerManager
	MessageSender         p2p.MessageSender
	// framework metastore prefix kvclient
	MetaKVClient metaclient.KVClient
	// user metastore raw kvclient
	UserRawKVClient       extKV.KVClientEx
	ExecutorClientManager client.ClientsManager
	ServerMasterClient    client.MasterClient
}

func NewBaseMaster(
	ctx *dcontext.Context,
	impl MasterImpl,
	id libModel.MasterID,
) BaseMaster {
	var (
		nodeID        p2p.NodeID
		advertiseAddr string
		masterMeta    = &libModel.MasterMetaKVData{}
		params        masterParams
	)
	if ctx != nil {
		nodeID = ctx.Environ.NodeID
		advertiseAddr = ctx.Environ.Addr
		metaBytes := ctx.Environ.MasterMetaBytes
		err := errors.Trace(masterMeta.Unmarshal(metaBytes))
		if err != nil {
			log.L().Warn("invalid master meta", zap.ByteString("data", metaBytes), zap.Error(err))
		}
	}

	if err := ctx.Deps().Fill(&params); err != nil {
		// TODO more elegant error handling
		log.L().Panic("failed to provide dependencies", zap.Error(err))
	}

	return &DefaultBaseMaster{
		Impl:                  impl,
		messageHandlerManager: params.MessageHandlerManager,
		messageSender:         params.MessageSender,
		metaKVClient:          params.MetaKVClient,
		userRawKVClient:       params.UserRawKVClient,
		executorClientManager: params.ExecutorClientManager,
		serverMasterClient:    params.ServerMasterClient,
		pool:                  workerpool.NewDefaultAsyncPool(4),
		id:                    id,
		clock:                 clock.New(),

		timeoutConfig: config.DefaultTimeoutConfig(),
		masterMeta:    masterMeta,

		errCh:   make(chan error, 1),
		closeCh: make(chan struct{}),

		uuidGen: uuid.NewGenerator(),

		nodeID:        nodeID,
		advertiseAddr: advertiseAddr,

		createWorkerQuota: quota.NewConcurrencyQuota(maxCreateWorkerConcurrency),
		// [TODO] use tenantID if support muliti-tenant
		userMetaKVClient: kvclient.NewPrefixKVClient(params.UserRawKVClient, tenant.DefaultUserTenantID),
		deps:             ctx.Deps(),
	}
}

func (m *DefaultBaseMaster) MetaKVClient() metaclient.KVClient {
	return m.userMetaKVClient
}

func (m *DefaultBaseMaster) Init(ctx context.Context) error {
	isInit, err := m.doInit(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	if isInit {
		if err := m.Impl.InitImpl(ctx); err != nil {
			return errors.Trace(err)
		}
	} else {
		if err := m.Impl.OnMasterRecovered(ctx); err != nil {
			return errors.Trace(err)
		}
	}

	if err := m.markStatusCodeInMetadata(ctx, libModel.MasterStatusInit); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (m *DefaultBaseMaster) doInit(ctx context.Context) (isFirstStartUp bool, err error) {
	isInit, epoch, err := m.refreshMetadata(ctx)
	if err != nil {
		return false, errors.Trace(err)
	}
	m.currentEpoch.Store(epoch)

	if err := m.deps.Provide(func() workerpool.AsyncPool {
		return m.pool
	}); err != nil {
		return false, errors.Trace(err)
	}

	m.workerManager = master.NewWorkerManager(
		m.id,
		epoch,
		m.metaKVClient,
		m.messageSender,
		func(_ context.Context, handle master.WorkerHandle) error {
			return m.Impl.OnWorkerOnline(handle)
		},
		func(_ context.Context, handle master.WorkerHandle, err error) error {
			return m.Impl.OnWorkerOffline(handle, err)
		},
		func(_ context.Context, handle master.WorkerHandle) error {
			return m.Impl.OnWorkerStatusUpdated(handle, handle.Status())
		},
		func(_ context.Context, handle master.WorkerHandle, err error) error {
			return m.Impl.OnWorkerDispatched(handle, err)
		}, isInit, m.timeoutConfig, m.clock)

	if err := m.registerMessageHandlers(ctx); err != nil {
		return false, errors.Trace(err)
	}

	m.startBackgroundTasks()
	return isInit, nil
}

func (m *DefaultBaseMaster) registerMessageHandlers(ctx context.Context) error {
	ok, err := m.messageHandlerManager.RegisterHandler(
		ctx,
		libModel.HeartbeatPingTopic(m.id),
		&libModel.HeartbeatPingMessage{},
		func(sender p2p.NodeID, value p2p.MessageValue) error {
			msg := value.(*libModel.HeartbeatPingMessage)
			log.L().Info("Heartbeat Ping received",
				zap.Any("msg", msg),
				zap.String("master-id", m.id))
			ok, err := m.messageSender.SendToNode(
				ctx,
				sender,
				libModel.HeartbeatPongTopic(m.id, msg.FromWorkerID),
				&libModel.HeartbeatPongMessage{
					SendTime:   msg.SendTime,
					ReplyTime:  m.clock.Now(),
					ToWorkerID: m.id,
					Epoch:      m.currentEpoch.Load(),
				})
			if err != nil {
				return err
			}
			if !ok {
				// TODO add a retry mechanism
				return nil
			}
			if err := m.workerManager.HandleHeartbeat(msg, sender); err != nil {
				return errors.Trace(err)
			}
			return nil
		})
	if err != nil {
		return err
	}
	if !ok {
		log.L().Panic("duplicate handler", zap.String("topic", libModel.HeartbeatPingTopic(m.id)))
	}

	ok, err = m.messageHandlerManager.RegisterHandler(
		ctx,
		statusutil.WorkerStatusTopic(m.id),
		&statusutil.WorkerStatusMessage{},
		func(sender p2p.NodeID, value p2p.MessageValue) error {
			msg := value.(*statusutil.WorkerStatusMessage)
			m.workerManager.OnWorkerStatusUpdateMessage(msg)
			return nil
		})
	if err != nil {
		return err
	}
	if !ok {
		log.L().Panic("duplicate handler", zap.String("topic", statusutil.WorkerStatusTopic(m.id)))
	}

	return nil
}

func (m *DefaultBaseMaster) Poll(ctx context.Context) error {
	if err := m.doPoll(ctx); err != nil {
		return errors.Trace(err)
	}

	if err := m.Impl.Tick(ctx); err != nil {
		return errors.Trace(err)
	}

	return nil
}

func (m *DefaultBaseMaster) doPoll(ctx context.Context) error {
	select {
	case err := <-m.errCh:
		if err != nil {
			return errors.Trace(err)
		}
	case <-m.closeCh:
		return derror.ErrMasterClosed.GenWithStackByArgs()
	default:
	}

	if err := m.messageHandlerManager.CheckError(ctx); err != nil {
		return errors.Trace(err)
	}
	return m.workerManager.Tick(ctx)
}

func (m *DefaultBaseMaster) MasterMeta() *libModel.MasterMetaKVData {
	return m.masterMeta
}

func (m *DefaultBaseMaster) MasterID() libModel.MasterID {
	return m.id
}

func (m *DefaultBaseMaster) GetWorkers() map[libModel.WorkerID]WorkerHandle {
	return m.workerManager.GetWorkers()
}

func (m *DefaultBaseMaster) doClose() {
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	close(m.closeCh)
	m.wg.Wait()
	if err := m.messageHandlerManager.Clean(closeCtx); err != nil {
		log.L().Warn("Failed to clean up message handlers",
			zap.String("master-id", m.id))
	}
}

func (m *DefaultBaseMaster) Close(ctx context.Context) error {
	if err := m.Impl.CloseImpl(ctx); err != nil {
		return errors.Trace(err)
	}

	m.doClose()
	return nil
}

func (m *DefaultBaseMaster) startBackgroundTasks() {
	cctx, cancel := context.WithCancel(context.Background())
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-m.closeCh
		cancel()
	}()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.pool.Run(cctx); err != nil {
			m.OnError(err)
		}
	}()
}

func (m *DefaultBaseMaster) OnError(err error) {
	if errors.Cause(err) == context.Canceled {
		// TODO think about how to gracefully handle cancellation here.
		log.L().Warn("BaseMaster is being canceled", zap.String("id", m.id), zap.Error(err))
		return
	}
	select {
	case m.errCh <- err:
	default:
	}
}

// refreshMetadata load and update metadata by current epoch, nodeID, advertiseAddr, etc.
// master meta is persisted before it is created, in this function we update some
// fileds to the current value, including epoch, nodeID and advertiseAddr.
func (m *DefaultBaseMaster) refreshMetadata(ctx context.Context) (isInit bool, epoch libModel.Epoch, err error) {
	metaClient := metadata.NewMasterMetadataClient(m.id, m.metaKVClient)

	masterMeta, err := metaClient.Load(ctx)
	if err != nil {
		return false, 0, err
	}

	epoch, err = m.metaKVClient.GenEpoch(ctx)
	if err != nil {
		return false, 0, err
	}

	// We should update the master data to reflect our current information
	masterMeta.Epoch = epoch
	masterMeta.Addr = m.advertiseAddr
	masterMeta.NodeID = m.nodeID

	if err := metaClient.Store(ctx, masterMeta); err != nil {
		return false, 0, errors.Trace(err)
	}

	m.masterMeta = masterMeta
	// isInit true means the master is created but has not been initialized.
	isInit = masterMeta.StatusCode == libModel.MasterStatusUninit

	return
}

func (m *DefaultBaseMaster) markStatusCodeInMetadata(
	ctx context.Context, code libModel.MasterStatusCode,
) error {
	metaClient := metadata.NewMasterMetadataClient(m.id, m.metaKVClient)
	masterMeta, err := metaClient.Load(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	masterMeta.StatusCode = code
	return metaClient.Store(ctx, masterMeta)
}

// prepareWorkerConfig extracts information from WorkerConfig into detail fields.
// - If workerType is master type, the config is a `*MasterMetaKVData` struct and
//   contains pre allocated maseter ID, and json marshalled config.
// - If workerType is worker type, the config is a user defined config struct, we
//   marshal it to byte slice as returned config, and generate a random WorkerID.
func (m *DefaultBaseMaster) prepareWorkerConfig(
	workerType libModel.WorkerType, config WorkerConfig,
) (rawConfig []byte, workerID libModel.WorkerID, err error) {
	switch workerType {
	case CvsJobMaster, FakeJobMaster, DMJobMaster:
		masterMeta, ok := config.(*libModel.MasterMetaKVData)
		if !ok {
			err = derror.ErrMasterInvalidMeta.GenWithStackByArgs(config)
			return
		}
		rawConfig = masterMeta.Config
		workerID = masterMeta.ID
	case WorkerDMDump, WorkerDMLoad, WorkerDMSync:
		var b bytes.Buffer
		err = toml.NewEncoder(&b).Encode(config)
		if err != nil {
			return
		}
		rawConfig = b.Bytes()
		workerID = m.uuidGen.NewString()
	default:
		rawConfig, err = json.Marshal(config)
		if err != nil {
			return
		}
		workerID = m.uuidGen.NewString()
	}
	return
}

func (m *DefaultBaseMaster) CreateWorker(
	workerType libModel.WorkerType,
	config WorkerConfig,
	cost model.RescUnit,
) (libModel.WorkerID, error) {
	log.L().Info("CreateWorker",
		zap.Int64("worker-type", int64(workerType)),
		zap.Any("worker-config", config),
		zap.String("master-id", m.id))

	quotaCtx, cancel := context.WithTimeout(context.Background(), createWorkerWaitQuotaTimeout)
	defer cancel()
	if err := m.createWorkerQuota.Consume(quotaCtx); err != nil {
		return "", derror.ErrMasterConcurrencyExceeded.Wrap(err)
	}

	configBytes, workerID, err := m.prepareWorkerConfig(workerType, config)
	if err != nil {
		return "", err
	}

	go func() {
		defer func() {
			m.createWorkerQuota.Release()
		}()

		requestCtx, cancel := context.WithTimeout(context.Background(), createWorkerTimeout)
		defer cancel()
		// This following API should be refined.
		resp, err := m.serverMasterClient.ScheduleTask(requestCtx, &pb.TaskSchedulerRequest{Tasks: []*pb.ScheduleTask{{
			Task: &pb.TaskRequest{
				Id: 0,
			},
			Cost: int64(cost),
		}}},
			// TODO (zixiong) make the timeout configurable
			time.Second*10)
		if err != nil {
			m.workerManager.OnCreatingWorkerFinished(workerID, err)
			return
		}

		schedule := resp.GetSchedule()
		if len(schedule) != 1 {
			log.L().Panic("unexpected schedule result", zap.Any("schedule", schedule))
		}
		executorID := model.ExecutorID(schedule[0].ExecutorId)

		m.workerManager.OnCreatingWorker(workerID, executorID)

		err = m.executorClientManager.AddExecutor(executorID, schedule[0].Addr)
		if err != nil {
			m.workerManager.OnCreatingWorkerFinished(workerID, err)
			return
		}

		executorClient := m.executorClientManager.ExecutorClient(executorID)
		executorResp, err := executorClient.Send(requestCtx, &client.ExecutorRequest{
			Cmd: client.CmdDispatchTask,
			Req: &pb.DispatchTaskRequest{
				TaskTypeId: int64(workerType),
				TaskConfig: configBytes,
				MasterId:   m.id,
				WorkerId:   workerID,
			},
		})
		if err != nil {
			// The executor may have already launched the worker.
			// TODO summarize the kind of errors that could be received
			// after success.
			m.workerManager.OnCreatingWorkerFinished(workerID, nil)
			return
		}
		dispatchTaskResp := executorResp.Resp.(*pb.DispatchTaskResponse)
		log.L().Info("Worker dispatched", zap.Any("master-id", m.id), zap.Any("response", dispatchTaskResp))
		errCode := dispatchTaskResp.GetErrorCode()
		if errCode != pb.DispatchTaskErrorCode_OK {
			err := errors.Errorf("dispatch worker failed with error code: %d", errCode)
			m.workerManager.OnCreatingWorkerFinished(workerID, err)
			return
		}
		m.workerManager.OnCreatingWorkerFinished(workerID, nil)
	}()

	return workerID, nil
}

func (m *DefaultBaseMaster) IsMasterReady() bool {
	return m.workerManager.IsInitialized()
}
