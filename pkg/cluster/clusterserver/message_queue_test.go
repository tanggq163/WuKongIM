package cluster

import (
	"testing"

	"github.com/WuKongIM/WuKongIM/pkg/wkserver/proto"
)

func TestMessageQueueCanBeCreated(t *testing.T) {
	q := newMessageQueue(8, false, 0, 0)
	if len(q.left) != 8 || len(q.right) != 8 {
		t.Errorf("unexpected size")
	}
}

func TestMessageCanBeAddedAndGet(t *testing.T) {
	q := newMessageQueue(8, false, 0, 0)
	for i := 0; i < 8; i++ {
		added, stopped := q.add(&proto.Message{})
		if !added || stopped {
			t.Errorf("failed to add")
		}
	}
	add, stopped := q.add(&proto.Message{})
	add2, stopped2 := q.add(&proto.Message{})
	if add || add2 {
		t.Errorf("failed to drop message")
	}
	if stopped || stopped2 {
		t.Errorf("unexpectedly stopped")
	}
	if q.idx != 8 {
		t.Errorf("unexpected idx %d", q.idx)
	}
	lr := q.leftInWrite
	q.get()
	if q.idx != 0 {
		t.Errorf("unexpected idx %d", q.idx)
	}
	if lr == q.leftInWrite {
		t.Errorf("lr flag not updated")
	}
	add, stopped = q.add(&proto.Message{})
	add2, stopped2 = q.add(&proto.Message{})
	if !add || !add2 {
		t.Errorf("failed to add message")
	}
	if stopped || stopped2 {
		t.Errorf("unexpectedly stopped")
	}
}

func TestAddMessageIsRateLimited(t *testing.T) {
	q := newMessageQueue(10000, false, 0, 1024)
	for i := 0; i < 10000; i++ {
		m := &proto.Message{
			Content: []byte("testtesttesttesttesttest"),
		}
		if q.rl.RateLimited() {
			added, stopped := q.add(m)
			if !added && !stopped {
				return
			}
		} else {
			sz := q.rl.Get()
			added, stopped := q.add(m)
			if added {
				if q.rl.Get() != sz+uint64(m.Size()) {
					t.Errorf("failed to update rate limit")
				}
			}
			if !added || stopped {
				t.Errorf("failed to add")
			}
		}
	}
	t.Fatalf("failed to observe any rate limited message")
}

func TestGetWillResetTheRateLimiterSize(t *testing.T) {
	q := newMessageQueue(10000, false, 0, 1024)
	for i := 0; i < 8; i++ {
		m := &proto.Message{
			Content: []byte("testtesttesttesttesttest"),
		}
		added, stopped := q.add(m)
		if !added && stopped {
			t.Fatalf("failed to add message")
		}
	}
	if q.rl.Get() == 0 {
		t.Errorf("rate limiter size is 0")
	}
	q.get()
	if q.rl.Get() != 0 {
		t.Fatalf("failed to reset the rate limiter")
	}
}
