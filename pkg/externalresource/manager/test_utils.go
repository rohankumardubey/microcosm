package manager

import (
	"context"
	"sync"

	libModel "github.com/hanfei1991/microcosm/lib/model"
	"github.com/hanfei1991/microcosm/model"
	resourcemeta "github.com/hanfei1991/microcosm/pkg/externalresource/resourcemeta/model"
	"github.com/hanfei1991/microcosm/pkg/notifier"
)

// MockExecutorInfoProvider implements ExecutorInfoProvider interface
type MockExecutorInfoProvider struct {
	mu          sync.RWMutex
	executorSet map[resourcemeta.ExecutorID]struct{}
	notifier    *notifier.Notifier[model.ExecutorID]
}

// NewMockExecutorInfoProvider creates a new MockExecutorInfoProvider instance
func NewMockExecutorInfoProvider() *MockExecutorInfoProvider {
	return &MockExecutorInfoProvider{
		executorSet: make(map[resourcemeta.ExecutorID]struct{}),
		notifier:    notifier.NewNotifier[model.ExecutorID](),
	}
}

// AddExecutor adds an executor to the mock.
func (p *MockExecutorInfoProvider) AddExecutor(executorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.executorSet[resourcemeta.ExecutorID(executorID)] = struct{}{}
}

// RemoveExecutor removes an executor from the mock.
func (p *MockExecutorInfoProvider) RemoveExecutor(executorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.executorSet, resourcemeta.ExecutorID(executorID))
	p.notifier.Notify(model.ExecutorID(executorID))
}

// HasExecutor returns whether the mock contains the given executor.
func (p *MockExecutorInfoProvider) HasExecutor(executorID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if _, exists := p.executorSet[resourcemeta.ExecutorID(executorID)]; exists {
		return true
	}
	return false
}

// ListExecutors lists all executors.
func (p *MockExecutorInfoProvider) ListExecutors() (ret []string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for id := range p.executorSet {
		ret = append(ret, string(id))
	}
	return
}

// WatchExecutors implements ExecutorManager.WatchExecutors
func (p *MockExecutorInfoProvider) WatchExecutors(
	ctx context.Context,
) ([]model.ExecutorID, *notifier.Receiver[model.ExecutorID], error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var executors []model.ExecutorID
	for id := range p.executorSet {
		executors = append(executors, id)
	}

	return executors, p.notifier.NewReceiver(), nil
}

// MockJobStatusProvider implements ExecutorManager interface
type MockJobStatusProvider struct {
	mu       sync.RWMutex
	jobInfos JobStatusesSnapshot
	notifier *notifier.Notifier[JobStatusChangeEvent]
}

// NewMockJobStatusProvider returns a new instance of MockJobStatusProvider.
func NewMockJobStatusProvider() *MockJobStatusProvider {
	return &MockJobStatusProvider{
		jobInfos: make(JobStatusesSnapshot),
		notifier: notifier.NewNotifier[JobStatusChangeEvent](),
	}
}

// SetJobStatus upserts the status of a given job.
func (jp *MockJobStatusProvider) SetJobStatus(jobID libModel.MasterID, status JobStatus) {
	jp.mu.Lock()
	defer jp.mu.Unlock()

	jp.jobInfos[jobID] = status
}

// RemoveJob removes a job from the mock.
func (jp *MockJobStatusProvider) RemoveJob(jobID libModel.MasterID) {
	jp.mu.Lock()
	defer jp.mu.Unlock()

	delete(jp.jobInfos, jobID)
	jp.notifier.Notify(JobStatusChangeEvent{
		EventType: JobRemovedEvent,
		JobID:     jobID,
	})
}

// GetJobStatuses implements the interface JobStatusProvider.
func (jp *MockJobStatusProvider) GetJobStatuses(ctx context.Context) (JobStatusesSnapshot, error) {
	jp.mu.RLock()
	defer jp.mu.RUnlock()

	return jp.jobInfos, nil
}

// WatchJobStatuses implements the interface JobStatusProvider.
func (jp *MockJobStatusProvider) WatchJobStatuses(
	ctx context.Context,
) (JobStatusesSnapshot, *notifier.Receiver[JobStatusChangeEvent], error) {
	jp.mu.Lock()
	defer jp.mu.Unlock()

	snapCopy := make(JobStatusesSnapshot, len(jp.jobInfos))
	for k, v := range jp.jobInfos {
		snapCopy[k] = v
	}

	return snapCopy, jp.notifier.NewReceiver(), nil
}
