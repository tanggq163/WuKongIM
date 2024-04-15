package reactor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/lni/goutils/syncutil"
	"go.uber.org/zap"
)

type ReactorSub struct {
	stopper  *syncutil.Stopper
	opts     *Options
	handlers *handlerList // 当前处理者集合

	wklog.Log

	tmpHandlers []*handler

	avdanceC chan struct{}
}

func NewReactorSub(index int, opts *Options) *ReactorSub {
	return &ReactorSub{
		stopper:     syncutil.NewStopper(),
		opts:        opts,
		handlers:    newHandlerList(),
		Log:         wklog.NewWKLog(fmt.Sprintf("ReactorSub[%d]", index)),
		tmpHandlers: make([]*handler, 0, 10000),
		avdanceC:    make(chan struct{}, 1),
	}
}

func (r *ReactorSub) Start() error {
	r.stopper.RunWorker(r.run)
	return nil
}

func (r *ReactorSub) Stop() {
	r.stopper.Stop()
}

func (r *ReactorSub) run() {

	var (
		tick = time.NewTicker(r.opts.TickInterval)
	)

	for {
		r.readyEvents()
		select {
		case <-tick.C:
			r.tick()
		case <-r.avdanceC:
		case <-r.stopper.ShouldStop():
			return
		}
	}
}

func (r *ReactorSub) proposeAndWait(ctx context.Context, handleKey string, logs []replica.Log) ([]*ProposeResult, error) {

	// -------------------- 初始化提案数据 --------------------
	handler := r.handlers.get(handleKey)
	if handler == nil {
		return nil, nil
	}
	ids := make([]uint64, 0, len(logs))
	for _, log := range logs {
		ids = append(ids, log.Id)
	}
	key := strconv.FormatUint(ids[len(ids)-1], 10)

	// -------------------- 获得等待提交提案的句柄 --------------------
	waitC := handler.addWait(ctx, key, ids)
	defer close(waitC)

	// -------------------- 添加提案请求 --------------------
	req := newProposeReq(key, logs)
	handler.addPropose(req)

	// -------------------- 等待提案结果 --------------------
	select {
	case items := <-waitC:
		return items, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.stopper.ShouldStop():
		return nil, ErrReactorSubStopped
	}

}

func (r *ReactorSub) propose(handleKey string, logs []replica.Log) error {
	// -------------------- 初始化提案数据 --------------------
	handler := r.handlers.get(handleKey)
	if handler == nil {
		return nil
	}
	ids := make([]uint64, 0, len(logs))
	for _, log := range logs {
		ids = append(ids, log.Id)
	}
	key := strconv.FormatUint(ids[len(ids)-1], 10)

	// -------------------- 添加提案请求 --------------------
	req := newProposeReq(key, logs)
	handler.addPropose(req)
	return nil
}

func (r *ReactorSub) advance() {
	select {
	case r.avdanceC <- struct{}{}:
	default:
	}
}

func (r *ReactorSub) readyEvents() {

	r.handlers.readHandlers(&r.tmpHandlers)

	for _, handler := range r.tmpHandlers {
		if !handler.isPrepared() {
			continue
		}
		r.handleReady(handler)
		r.handleEvent(handler)
	}
	r.tmpHandlers = r.tmpHandlers[:0]
}

func (r *ReactorSub) handleReady(handler *handler) bool {
	rd := handler.ready()
	if replica.IsEmptyReady(rd) {
		return false
	}

	if !replica.IsEmptyHardState(rd.HardState) {
		handler.setHardState(rd.HardState)
	}

	for _, m := range rd.Messages {
		if m.To == r.opts.NodeId {
			r.handleLocalMsg(handler, m)
			continue
		}
		if m.To == 0 {
			r.Error("msg.To is 0", zap.String("msg", m.MsgType.String()))
			continue
		}

		if m.MsgType == replica.MsgSync {
			switch handler.sync.syncStatus {
			case syncStatusNone:
				handler.sync.startSyncTime = time.Now()
				handler.sync.syncingLogIndex = m.Index
				handler.sync.syncStatus = syncStatusSyncing
			case syncStatusSyncing:
				if m.Index > handler.sync.syncingLogIndex || time.Since(handler.sync.startSyncTime) > handler.sync.syncTimeout {
					handler.sync.syncingLogIndex = m.Index
					handler.sync.startSyncTime = time.Now()
					r.Warn("sync timeout...", zap.Uint64("index", m.Index), zap.Uint64("to", m.To))
				} else {
					r.Debug("syncing...", zap.Uint64("index", m.Index))
					continue
				}
			case syncStatusSynced:
				r.Warn("synced...", zap.Uint64("index", m.Index))
				continue
			}
		}

		r.opts.Send(Message{
			HandlerKey: handler.key,
			Message:    m,
		})
	}
	return true
}

func (r *ReactorSub) handleEvent(handler *handler) (bool, error) {
	var (
		hasEvent = false
		start    time.Time
		end      time.Duration
		err      error
		handleOk bool
	)

	// -------------------- 处理提案请求 --------------------
	if r.opts.EnableLazyCatchUp {
		start = time.Now()
	}
	handleOk, err = r.handleProposes(handler)
	if r.opts.EnableLazyCatchUp {
		end = time.Since(start)
		if end > time.Millisecond*1 {
			r.Info("handleProposes", zap.Duration("time", end))
		}
	}
	if err != nil {
		return false, err
	}
	if handleOk {
		hasEvent = true
	}

	// -------------------- 处理追加日志任务结果 --------------------
	if r.opts.EnableLazyCatchUp {
		start = time.Now()
	}
	handleOk, err = r.handleAppendLogTask(handler)
	if r.opts.EnableLazyCatchUp {
		end = time.Since(start)
		if end > time.Millisecond*1 {
			r.Info("handleAppendLogTask", zap.Duration("time", end))
		}
	}

	if err != nil {
		return false, err
	}
	if handleOk {
		hasEvent = true
	}

	// -------------------- 处理收到的消息 --------------------
	if r.opts.EnableLazyCatchUp {
		start = time.Now()
	}
	handleOk = r.handleRecvMessages(handler)
	if r.opts.EnableLazyCatchUp {
		end = time.Since(start)
		if end > time.Millisecond*1 {
			r.Info("handleRecvMessages", zap.Duration("time", end))
		}
	}
	if handleOk {
		hasEvent = true
	}

	// -------------------- 处理同步日志任务结果 --------------------
	if r.opts.EnableLazyCatchUp {
		start = time.Now()
	}

	handleOk, err = r.handleSyncTask(handler)
	if r.opts.EnableLazyCatchUp {
		end = time.Since(start)
		if end > time.Millisecond*1 {
			r.Info("handleSyncTask", zap.Duration("time", end))
		}
	}
	if err != nil {
		return false, err
	}
	if handleOk {
		hasEvent = true
	}

	// -------------------- 处理获取日志任务结果 --------------------
	if r.opts.EnableLazyCatchUp {
		start = time.Now()
	}
	handleOk, err = r.handleGetLogTask(handler)
	if r.opts.EnableLazyCatchUp {
		end = time.Since(start)
		if end > time.Millisecond*1 {
			r.Info("handleGetLogTask", zap.Duration("time", end))
		}
	}

	if err != nil {
		return false, err
	}
	if handleOk {
		hasEvent = true
	}

	// -------------------- 处理应用日志任务结果 --------------------
	if r.opts.EnableLazyCatchUp {
		start = time.Now()
	}
	handleOk, err = r.handleApplyLogTask(handler)
	if r.opts.EnableLazyCatchUp {
		end = time.Since(start)
		if end > time.Millisecond*1 {
			r.Info("handleApplyLogTask", zap.Duration("time", end))
		}
	}
	if err != nil {
		return false, err
	}
	if handleOk {
		hasEvent = true
	}
	return hasEvent, nil
}

func (r *ReactorSub) handleProposes(handler *handler) (bool, error) {
	var (
		ok              bool = true
		proposeReq      proposeReq
		proposeLogCount = 0 // 每批提案的日志数量
		hasEvent        bool
	)

	for ok {
		proposeReq, ok = handler.popPropose()
		if !ok {
			break
		}
		if len(proposeReq.logs) == 0 {
			continue
		}

		proposeLogCount += len(proposeReq.logs)
		lastLogIndex, term := handler.lastLogIndexAndTerm()
		for i := 0; i < len(proposeReq.logs); i++ {
			lg := proposeReq.logs[i]
			lg.Index = lastLogIndex + 1 + uint64(i)
			lg.Term = term
			proposeReq.logs[i] = lg
			handler.didPropose(proposeReq.key, lg.Id, lg.Index)
		}

		err := handler.step(replica.NewProposeMessageWithLogs(r.opts.NodeId, term, proposeReq.logs))
		if err != nil {
			r.Error("step propose log failed", zap.Error(err))
			return false, err
		}
		hasEvent = true
		if r.opts.MaxProposeLogCount > 0 && proposeLogCount > r.opts.MaxProposeLogCount {
			break
		}
	}
	return hasEvent, nil
}

func (r *ReactorSub) handleAppendLogTask(handler *handler) (bool, error) {
	firstTask := handler.firstAppendLogStoreTask()
	var (
		hasEvent bool
	)
	for firstTask != nil {
		if !firstTask.isTaskFinished() {
			break
		}
		if firstTask.hasErr() {
			r.Panic("append log store message failed", zap.Error(firstTask.err()))
			return false, firstTask.err()
		}
		err := handler.step(firstTask.resp())
		if err != nil {
			r.Panic("step local store message failed", zap.Error(err))
			return false, err
		}
		err = handler.setLastIndex(firstTask.resp().Index) // TODO: 耗时操作，不应该放到ready里执行，后续要优化
		if err != nil {
			r.Panic("set last index failed", zap.Error(err))
			return false, err
		}
		hasEvent = true
		handler.removeFirstAppendLogStoreTask()
		firstTask = handler.firstAppendLogStoreTask()
	}
	return hasEvent, nil
}

func (r *ReactorSub) handleRecvMessages(handler *handler) bool {
	// 处理收到的消息
	msgs := handler.getMessages()
	for _, msg := range msgs {
		if msg.MsgType == replica.MsgSyncResp {
			handler.setMsgSyncResp(msg.Message)
			continue
		}
		err := handler.step(msg.Message)
		if err != nil {
			r.Warn("step error", zap.Error(err))
		}
	}
	return false
}

func (r *ReactorSub) handleSyncTask(handler *handler) (bool, error) {
	msgSyncResp, ok := handler.getAndResetMsgSyncResp()
	if ok {
		err := handler.step(msgSyncResp)
		if err != nil {
			r.Error("step sync message failed", zap.Error(err))
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (r *ReactorSub) handleGetLogTask(handler *handler) (bool, error) {
	var (
		err      error
		hasEvent bool
	)
	getLogsTasks := handler.allGetLogsTask()
	for _, getLogsTask := range getLogsTasks {
		if getLogsTask.isTaskFinished() {
			if getLogsTask.hasErr() {
				r.Error("get logs task error", zap.Error(getLogsTask.err()))
			} else {
				err = handler.step(getLogsTask.resp())
				if err != nil {
					r.Error("step get logs task failed", zap.Error(err))
				}
			}
			handler.removeGetLogsTask(getLogsTask.taskKey())
			hasEvent = true
		}
	}
	return hasEvent, nil
}

func (r *ReactorSub) handleApplyLogTask(handler *handler) (bool, error) {
	firstTask := handler.firstApplyLogStoreTask()
	var (
		hasEvent bool
	)
	for firstTask != nil {
		if !firstTask.isTaskFinished() {
			break
		}
		if firstTask.hasErr() {
			r.Panic("apply log store message failed", zap.Error(firstTask.err()))
			return false, firstTask.err()
		}
		resp := firstTask.resp()
		err := handler.step(resp)
		if err != nil {
			r.Panic("step apply store message failed", zap.Error(err))
			return false, err
		}
		err = handler.setAppliedIndex(resp.AppliedIndex) // TODO: 耗时操作，不应该放到ready里执行，后续要优化
		if err != nil {
			r.Panic("set applied index failed", zap.Error(err))
			return false, err
		}
		hasEvent = true
		handler.removeFirstApplyLogStoreTask()
		firstTask = handler.firstApplyLogStoreTask()
	}
	return hasEvent, nil
}

func (r *ReactorSub) tick() {
	r.handlers.readHandlers(&r.tmpHandlers)

	for _, handler := range r.tmpHandlers {
		handler.tick()
	}
	r.tmpHandlers = r.tmpHandlers[:0]
}

func (r *ReactorSub) handleLocalMsg(handler *handler, msg replica.Message) {
	switch msg.MsgType {
	case replica.MsgSyncGet:
		r.handleSyncGet(handler, msg)
	case replica.MsgStoreAppend:
		r.handleStoreAppend(handler, msg)
	case replica.MsgApplyLogsReq:
		r.handleApplyLogsReq(handler, msg)
	case replica.MsgVoteResp:
		err := handler.step(msg)
		if err != nil {
			r.Error("step vote resp message failed", zap.Error(err))
		}
	}
}

func (r *ReactorSub) handleSyncGet(handler *handler, msg replica.Message) {
	if msg.Index <= 0 {
		return
	}
	r.Debug("query logs", zap.Uint64("index", msg.Index), zap.Uint64("from", msg.From))
	tk := newGetLogsTask(msg.From, msg.Index)

	tk.setExec(func() error {
		resultLogs, err := handler.getAndMergeLogs(msg)
		if err != nil {
			r.Error("get logs error", zap.Error(err))
		}
		resp := replica.NewMsgSyncGetResp(r.opts.NodeId, msg.From, msg.Index, resultLogs)
		tk.setResp(resp)
		tk.taskFinished()
		if len(resultLogs) > 0 {
			r.advance()
		}
		return nil
	})
	handler.addGetLogsTask(tk)
}

func (r *ReactorSub) handleStoreAppend(handler *handler, msg replica.Message) {
	if len(msg.Logs) == 0 {
		return
	}
	lastLog := msg.Logs[len(msg.Logs)-1]
	tk := newStoreAppendTask(lastLog.Index)
	tk.setExec(func() error {
		err := handler.appendLogs(msg.Logs)
		if err != nil {
			r.Panic("append logs error", zap.Error(err))
			return err
		}
		tk.setResp(replica.NewMsgStoreAppendResp(r.opts.NodeId, lastLog.Index))
		tk.taskFinished()
		r.advance()
		return nil
	})
	handler.addAppendLogStoreTask(tk)
}

func (r *ReactorSub) handleApplyLogsReq(handler *handler, msg replica.Message) {
	applyingIndex := msg.ApplyingIndex
	committedIndex := msg.CommittedIndex
	if applyingIndex > committedIndex {
		r.Debug("not apply logs req", zap.Uint64("applyingIndex", applyingIndex), zap.Uint64("committedIndex", committedIndex))
		return
	}
	if msg.CommittedIndex == 0 {
		r.Panic("committedIndex is 0", zap.Uint64("applyingIndex", applyingIndex), zap.Uint64("committedIndex", committedIndex))
	}

	handler.didCommit(msg.ApplyingIndex+1, msg.CommittedIndex+1)

	tk := newApplyLogsTask(committedIndex)
	tk.setExec(func() error {
		logs, err := handler.logs(msg.ApplyingIndex+1, msg.CommittedIndex+1, 0)
		if err != nil {
			r.Panic("get logs error", zap.Error(err))
			return err
		}
		err = handler.applyLogs(logs)
		if err != nil {
			r.Panic("apply logs error", zap.Error(err))
			return err
		}
		tk.setResp(replica.NewMsgApplyLogsRespMessage(r.opts.NodeId, msg.Term, committedIndex))
		tk.taskFinished()
		r.advance()
		return nil
	})
	handler.addApplyLogStoreTask(tk)
}

func (r *ReactorSub) addHandler(handler *handler) {

	r.handlers.add(handler)

}

func (r *ReactorSub) removeHandler(key string) *handler {

	return r.handlers.remove(key)
}

func (r *ReactorSub) handler(key string) *handler {

	return r.handlers.get(key)
}

func (r *ReactorSub) addMessage(m Message) {
	handler := r.handlers.get(m.HandlerKey)
	if handler == nil {
		r.Warn("handler not exist", zap.String("handlerKey", m.HandlerKey))
		return
	}
	handler.addMessage(m)
}
