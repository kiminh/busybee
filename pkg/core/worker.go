package core

import (
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/deepfabric/busybee/pkg/expr"
	"github.com/deepfabric/busybee/pkg/metric"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/queue"
	"github.com/deepfabric/busybee/pkg/storage"
	bbutil "github.com/deepfabric/busybee/pkg/util"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/format"
	"github.com/fagongzi/util/hack"
	"github.com/fagongzi/util/protoc"
	"github.com/fagongzi/util/task"
	"github.com/robfig/cron/v3"
)

var (
	workerBatch = int64(16)
)

const (
	stopAction = iota
	timerAction
	userEventAction
	updateCrowdEventAction
	updateWorkflowEventAction
)

type directCtx struct {
}

type item struct {
	action int
	value  interface{}
	cb     *stepCB
}

type stateWorker struct {
	stopped          uint32
	key              string
	eng              Engine
	buf              *goetty.ByteBuf
	state            metapb.WorkflowInstanceWorkerState
	totalCrowds      *roaring.Bitmap
	stepCrowds       []*roaring.Bitmap
	directSteps      map[string]string
	steps            map[string]excution
	entryActions     map[string]string
	leaveActions     map[string]string
	queue            *task.Queue
	cronIDs          []cron.EntryID
	consumer         queue.Consumer
	tenant           string
	cond             *rpcpb.Condition
	conditionKey     []byte
	queueStateKey    []byte
	queueGetStateKey []byte
}

func newStateWorker(key string, state metapb.WorkflowInstanceWorkerState, eng Engine) (*stateWorker, error) {
	consumer, err := queue.NewConsumer(state.TenantID, eng.Storage(), []byte(key))
	if err != nil {
		metric.IncStorageFailed()
		return nil, err
	}

	queueStateKey := make([]byte, 12, 12)
	goetty.Uint64ToBytesTo(state.InstanceID, queueStateKey)
	goetty.Uint32ToBytesTo(state.Index, queueStateKey[8:])

	w := &stateWorker{
		key:              key,
		state:            state,
		eng:              eng,
		buf:              goetty.NewByteBuf(32),
		totalCrowds:      acquireBM(),
		queue:            task.New(1024),
		consumer:         consumer,
		tenant:           string(format.UInt64ToString(state.TenantID)),
		cond:             &rpcpb.Condition{},
		conditionKey:     make([]byte, len(queueStateKey)+1, len(queueStateKey)+1),
		queueStateKey:    queueStateKey,
		queueGetStateKey: storage.PartitionKVKey(state.TenantID, 0, queueStateKey),
	}

	err = w.resetByState()
	if err != nil {
		metric.IncWorkflowWorkerFailed()
		return nil, err
	}

	return w, nil
}

func (w *stateWorker) resetByState() error {
	w.totalCrowds.Clear()
	w.stepCrowds = w.stepCrowds[:0]
	w.directSteps = make(map[string]string)
	w.steps = make(map[string]excution)
	w.entryActions = make(map[string]string)
	w.leaveActions = make(map[string]string)

	for idx, stepState := range w.state.States {
		bm := acquireBM()
		bbutil.MustParseBMTo(stepState.LoaderMeta, bm)
		w.stepCrowds = append(w.stepCrowds, bm)
		w.totalCrowds.Or(bm)

		exec, err := newExcution(stepState.Step.Name, stepState.Step.Execution)
		if err != nil {
			return err
		}

		if stepState.Step.Execution.Timer != nil {
			i := idx
			id, err := w.eng.AddCronJob(stepState.Step.Execution.Timer.Cron, func() {
				w.queue.Put(item{
					action: timerAction,
					value:  i,
				})
			})
			if err != nil {
				metric.IncWorkflowWorkerFailed()
				return err
			}

			w.cronIDs = append(w.cronIDs, id)
		}

		w.steps[stepState.Step.Name] = exec
		w.entryActions[stepState.Step.Name] = stepState.Step.EnterAction
		w.leaveActions[stepState.Step.Name] = stepState.Step.LeaveAction

		if stepState.Step.Execution.Type == metapb.Direct &&
			stepState.Step.Execution.Direct.NextStep != "" {
			w.directSteps[stepState.Step.Name] = stepState.Step.Execution.Direct.NextStep
		}
	}

	return nil
}

func (w *stateWorker) stop() {
	atomic.StoreUint32(&w.stopped, 1)
	w.queue.Put(item{
		action: stopAction,
	})
}

func (w *stateWorker) isStopped() bool {
	return atomic.LoadUint32(&w.stopped) == 1
}

func (w *stateWorker) matches(uid uint32) bool {
	return w.totalCrowds.Contains(uid)
}

func (w *stateWorker) onEvent(maxOffset uint64, items ...[]byte) (uint64, error) {
	logger.Debugf("worker %s consumer from queue, last offset %d, %d items",
		w.key,
		maxOffset,
		len(items))

	offset := maxOffset - uint64(len(items)) + 1
	completed := offset - 1

	var event metapb.Event
	var events []metapb.UserEvent
	for _, item := range items {
		event.Reset()
		protoc.MustUnmarshal(&event, item)
		logger.Debugf("worker %s consumer event %+v",
			w.key,
			event)

		switch event.Type {
		case metapb.UserType:
			if w.matches(event.User.UserID) {
				evt := *event.User
				evt.WorkflowID = w.state.WorkflowID
				evt.InstanceID = w.state.InstanceID
				events = append(events, evt)
			}
		case metapb.UpdateCrowdType:
			if event.UpdateCrowd.WorkflowID == w.state.WorkflowID &&
				event.UpdateCrowd.Index == w.state.Index {
				if len(events) > 0 {
					err := w.doEvent(userEventAction, events)
					if err != nil {
						return completed, err
					}

					completed = offset - 1
					events = events[:0]
				}

				err := w.doEvent(updateCrowdEventAction, event.UpdateCrowd.Crowd)
				if err != nil {
					return completed, err
				}
				completed = offset
			}
		case metapb.UpdateWorkflowType:
			if event.UpdateWorkflow.Workflow.ID == w.state.WorkflowID {
				if len(events) > 0 {
					err := w.doEvent(userEventAction, events)
					if err != nil {
						return completed, err
					}

					completed = offset - 1
					events = events[:0]
				}

				err := w.doEvent(updateWorkflowEventAction, event.UpdateWorkflow.Workflow)
				if err != nil {
					return completed, err
				}
				completed = offset
			}
		}

		offset++
	}

	if len(events) > 0 {
		err := w.doEvent(userEventAction, events)
		if err != nil {
			return completed, err
		}
	}

	if len(items) > 0 {
		metric.IncEventHandled(len(items), w.tenant, metapb.TenantInputGroup)
	}

	return maxOffset, nil
}

func (w *stateWorker) doEvent(action int, data interface{}) error {
	cb := acquireCB()
	defer releaseCB(cb)

	err := w.queue.Put(item{
		action: action,
		value:  data,
		cb:     cb,
	})
	if err != nil {
		return err
	}

	return cb.wait()
}

func (w *stateWorker) run() {
	go func() {
		w.checkLastTranscation()

		items := make([]interface{}, workerBatch, workerBatch)
		tran := newTransaction()

		for {
			n, err := w.queue.Get(workerBatch, items)
			if err != nil {
				logger.Fatalf("BUG: fetch from work queue failed with %+v", err)
			}

			w.buf.Clear()
			tran.start(w)
			for i := int64(0); i < n; i++ {
				value := items[i].(item)
				if value.action == stopAction {
					w.consumer.Stop()

					for _, v := range w.queue.Dispose() {
						(v.(item).cb).complete(task.ErrDisposed)
					}

					for _, id := range w.cronIDs {
						w.eng.StopCronJob(id)
					}

					for _, bm := range w.stepCrowds {
						releaseBM(bm)
					}
					releaseBM(w.totalCrowds)
					logger.Infof("worker %s stopped", w.key)
					return
				}

				switch value.action {
				case timerAction:
					tran.doStepTimerEvent(value)
				case userEventAction:
					tran.doStepUserEvents(value)
				case updateCrowdEventAction:
					err = tran.err
					w.completeTransaction(tran)
					if err != nil {
						value.cb.complete(err)
					} else {
						value.cb.complete(w.doUpdateCrowd(value.value.([]byte)))
					}

					tran.start(w)
				case updateWorkflowEventAction:
					err = tran.err
					w.completeTransaction(tran)
					if err != nil {
						value.cb.complete(err)
					} else {
						value.cb.complete(w.doUpdateWorkflow(value.value.(metapb.Workflow)))
					}

					tran.start(w)
				}

				if err != nil {
					break
				}
			}

			w.completeTransaction(tran)
		}
	}()

	w.consumer.Start(uint64(workerBatch), 0, w.onEvent)
	logger.Infof("worker %s started", w.key)
}

func (w *stateWorker) completeTransaction(tran *transaction) {
	defer tran.reset()

	if tran.err != nil {
		for _, c := range tran.cbs {
			c.complete(tran.err)
		}
		return
	}

	if len(tran.changes) > 0 {
		for idx := range w.stepCrowds {
			w.stepCrowds[idx].Clear()
			w.stepCrowds[idx].Or(tran.stepCrowds[idx])
		}

		w.state.Version++
		w.retryDo("exec notify", tran, w.execNotify)
		w.retryDo("exec update state", tran, w.execUpdate)

		logger.Infof("worker %s state update to version %d",
			w.key,
			w.state.Version)
	}

	for _, c := range tran.cbs {
		c.complete(nil)
	}
}

func (w *stateWorker) checkLastTranscation() {
	// Every transcation will wirte a newest state to the store if crowd changed,
	// check the last version and the current, and do:
	// 1. last verison <= current version, transaction completed, remove transaction
	// 2. last verison > current version, last notify changed already added, but state was not added
	for {
		value, err := w.eng.Storage().GetWithGroup(w.queueGetStateKey, metapb.TenantOutputGroup)
		if err != nil {
			logger.Errorf("worker %s load last transaction failed with %+v, retry later",
				w.key,
				err)
			time.Sleep(time.Second * 1)
			continue
		}

		if len(value) == 0 {
			logger.Infof("worker %s has no last transcation",
				w.key)
			break
		}

		last := metapb.WorkflowInstanceWorkerState{}
		protoc.MustUnmarshal(&last, value)
		if last.Version <= w.state.Version {
			break
		}

		w.state = last
		err = w.resetByState()
		if err != nil {
			logger.Fatalf("worker %s reset state failed with %+v",
				w.key, err)
		}

		w.retryDo("update state by last trnasaction", nil, w.execUpdate)
		break
	}
}

func (w *stateWorker) execNotify(tran *transaction) error {
	notifies := make([]metapb.Notify, 0, len(tran.changes))
	for _, changed := range tran.changes {
		nt := metapb.Notify{
			TenantID:   w.state.TenantID,
			WorkflowID: w.state.WorkflowID,
			InstanceID: w.state.InstanceID,
			UserID:     changed.user(),
			Crowd:      changed.crowd(),
			FromStep:   changed.from,
			ToStep:     changed.to,
			TTL:        changed.ttl,
			FromAction: w.leaveActions[changed.from],
			ToAction:   w.entryActions[changed.to],
		}
		notifies = append(notifies, nt)

		if logger.DebugEnabled() {
			logger.Debugf("worker %s notify to %d users, notify %+v",
				w.key,
				changed.who.users.GetCardinality(),
				nt)
		}
	}

	w.buf.MarkWrite()
	w.buf.WriteUInt64(w.state.Version)
	condValue := w.buf.WrittenDataAfterMark()

	w.cond.Reset()
	w.cond.Key = w.conditionKey
	w.cond.Value = condValue
	w.cond.Cmp = rpcpb.GT
	return w.eng.Notifier().Notify(w.state.TenantID, w.buf, notifies, w.cond,
		w.conditionKey, condValue,
		w.queueStateKey, protoc.MustMarshal(&w.state))
}

func (w *stateWorker) execUpdate(batch *transaction) error {
	for idx := range w.state.States {
		w.state.States[idx].Loader = metapb.RawLoader
		w.state.States[idx].LoaderMeta = bbutil.MustMarshalBM(w.stepCrowds[idx])
		w.state.States[idx].TotalCrowd = w.stepCrowds[idx].GetCardinality()
	}

	req := rpcpb.AcquireUpdateInstanceStateShardRequest()
	req.State = w.state
	_, err := w.eng.Storage().ExecCommand(req)
	return err
}

func (w *stateWorker) doUpdateWorkflow(workflow metapb.Workflow) error {
	for _, id := range w.cronIDs {
		w.eng.StopCronJob(id)
	}
	w.cronIDs = w.cronIDs[:0]

	oldCrowds := make(map[string]*roaring.Bitmap)
	for idx, step := range w.state.States {
		oldCrowds[step.Step.Name] = w.stepCrowds[idx]
	}

	w.directSteps = make(map[string]string)
	w.steps = make(map[string]excution)
	w.entryActions = make(map[string]string)
	w.leaveActions = make(map[string]string)

	var newCrowds []*roaring.Bitmap
	var newStates []metapb.StepState
	for idx, step := range workflow.Steps {
		exec, err := newExcution(step.Name, step.Execution)
		if err != nil {
			return err
		}

		if step.Execution.Timer != nil {
			i := idx
			id, err := w.eng.AddCronJob(step.Execution.Timer.Cron, func() {
				w.queue.Put(item{
					action: timerAction,
					value:  i,
				})
			})
			if err != nil {
				return err
			}

			w.cronIDs = append(w.cronIDs, id)
		}

		w.steps[step.Name] = exec
		w.entryActions[step.Name] = step.EnterAction
		w.leaveActions[step.Name] = step.LeaveAction

		if step.Execution.Type == metapb.Direct &&
			step.Execution.Direct.NextStep != "" {
			w.directSteps[step.Name] = step.Execution.Direct.NextStep
		}

		newBM := acquireBM()
		if bm, ok := oldCrowds[step.Name]; ok {
			newBM.Or(bm)
		}

		newCrowds = append(newCrowds, newBM)
		newStates = append(newStates, metapb.StepState{
			Step: step,
		})
	}

	for _, bm := range w.stepCrowds {
		releaseBM(bm)
	}

	w.stepCrowds = newCrowds
	w.state.States = newStates
	w.state.Version++
	w.retryDo("exec update workflow", nil, w.execUpdate)
	logger.Infof("worker %s workflow updated", w.key)
	return nil
}

func (w *stateWorker) doUpdateCrowd(crowd []byte) error {
	newTotal := acquireBM()
	defer releaseBM(newTotal)
	bbutil.MustParseBMTo(crowd, newTotal)

	newAdded := acquireBM()
	defer releaseBM(newAdded)
	newAdded.Or(newTotal)
	newAdded.AndNot(w.totalCrowds)

	w.totalCrowds.Clear()
	w.totalCrowds.Or(newTotal)

	for idx, sc := range w.stepCrowds {
		if idx == 0 {
			sc.Or(newAdded)
		}
		sc.And(newTotal)
	}

	w.state.Version++
	w.retryDo("exec update crowd", nil, w.execUpdate)
	logger.Infof("worker %s crowd updated", w.key)
	return nil
}

func (w *stateWorker) isDirectStep(name string) bool {
	_, ok := w.directSteps[name]
	return ok
}

func (w *stateWorker) retryDo(thing string, tran *transaction, fn func(*transaction) error) {
	times := 1
	after := 2
	maxAfter := 30
	for {
		if w.isStopped() {
			return
		}

		err := fn(tran)
		if err == nil {
			return
		}

		metric.IncStorageFailed()
		logger.Errorf("worker %s do %s failed %d times with %+v, retry after %d sec",
			w.key,
			thing,
			times,
			err,
			after)
		times++
		if after < maxAfter {
			after = after * 2
			if after > maxAfter {
				after = maxAfter
			}
		}
		time.Sleep(time.Second * time.Duration(after))
	}
}

type exprCtx struct {
	event metapb.UserEvent
	w     *stateWorker
	idx   int
}

func newExprCtx(event metapb.UserEvent, w *stateWorker, idx int) expr.Ctx {
	return &exprCtx{
		w:     w,
		idx:   idx,
		event: event,
	}
}

func (c *exprCtx) Event() metapb.UserEvent {
	return c.event
}

func (c *exprCtx) Profile(key []byte) ([]byte, error) {
	value, err := c.w.eng.Service().GetProfileField(c.event.TenantID, c.event.UserID, hack.SliceToString(key))
	if err != nil {
		return nil, err
	}

	return value, nil
}

func (c *exprCtx) KV(key []byte) ([]byte, error) {
	return c.w.eng.Storage().Get(key)
}

func (c *exprCtx) TotalCrowd() *roaring.Bitmap {
	return c.w.totalCrowds
}

func (c *exprCtx) StepCrowd() *roaring.Bitmap {
	return c.w.stepCrowds[c.idx]
}

func (c *exprCtx) StepTTL() ([]byte, error) {
	name := c.w.state.States[c.idx].Step.Name
	key := storage.WorkflowStepTTLKey(c.event.WorkflowID, c.event.UserID, name, c.w.buf)
	v, err := c.w.eng.Storage().Get(key)
	if err != nil {
		return nil, err
	}

	return v, nil
}
