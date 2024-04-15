package replica

import "time"

type AckMode int

const (
	// AckModeNone AckModeNone
	AckModeNone AckMode = iota
	// AckModeMajority AckModeMajority
	AckModeMajority
	// AckModeAll AckModeAll
	AckModeAll
)

type Options struct {
	NodeId                uint64   // 当前节点ID
	Replicas              []uint64 // 副本节点ID集合
	Storage               IStorage
	MaxUncommittedLogSize uint64
	AppliedIndex          uint64        // 已应用的日志下标
	SyncLimitSize         uint64        // 每次同步日志数据的最大大小（过小影响吞吐量，过大导致消息阻塞，默认为4M）
	MessageSendInterval   time.Duration // 消息发送间隔
	ActiveReplicaMaxTick  int           // 最大空闲时间
	AckMode               AckMode       // AckMode
	ReplicaMaxCount       int           // 副本最大数量
	ElectionOn            bool          // 是否开启选举
	ElectionTimeoutTick   int           // 选举超时tick次数，超过次tick数则发起选举
	HeartbeatTimeoutTick  int           // 心跳超时tick次数, 就是tick触发几次算一次心跳，一般为1 一次tick算一次心跳
	// LastSyncInfoMap       map[uint64]*SyncInfo
}

func NewOptions() *Options {
	return &Options{
		MaxUncommittedLogSize: 1024 * 1024 * 1024,
		SyncLimitSize:         1024 * 1024 * 20, // 20M
		// LastSyncInfoMap:       map[uint64]*SyncInfo{},
		MessageSendInterval:  time.Millisecond * 150,
		ActiveReplicaMaxTick: 10,
		AckMode:              AckModeMajority,
		ReplicaMaxCount:      3,
		ElectionOn:           false,
		ElectionTimeoutTick:  10,
		HeartbeatTimeoutTick: 2,
	}
}

type Option func(o *Options)

func WithReplicas(replicas []uint64) Option {
	return func(o *Options) {
		o.Replicas = replicas
	}
}

func WithStorage(storage IStorage) Option {
	return func(o *Options) {
		o.Storage = storage
	}
}

func WithMaxUncommittedLogSize(size uint64) Option {
	return func(o *Options) {
		o.MaxUncommittedLogSize = size
	}
}

func WithAppliedIndex(index uint64) Option {
	return func(o *Options) {
		o.AppliedIndex = index
	}
}

func WithSyncLimitSize(size uint64) Option {
	return func(o *Options) {
		o.SyncLimitSize = size
	}
}

func WithMessageSendInterval(interval time.Duration) Option {
	return func(o *Options) {
		o.MessageSendInterval = interval
	}
}

func WithAckMode(ackMode AckMode) Option {
	return func(o *Options) {
		o.AckMode = ackMode
	}
}

func WithReplicaMaxCount(count int) Option {
	return func(o *Options) {
		o.ReplicaMaxCount = count
	}
}

func WithElectionOn(electionOn bool) Option {
	return func(o *Options) {
		o.ElectionOn = electionOn
	}
}

func WithElectionTimeoutTick(tick int) Option {
	return func(o *Options) {
		o.ElectionTimeoutTick = tick
	}
}

func WithHeartbeatTimeoutTick(tick int) Option {
	return func(o *Options) {
		o.HeartbeatTimeoutTick = tick
	}
}
