package ec2wrapper

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/errors"
	"gotest.tools/assert"
)

func TestHedgeBasic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int64
	delays := []time.Duration{time.Duration(0), time.Second}
	resp, err := hedge(ctx, func(ctx context.Context) (interface{}, error) {
		atomic.AddInt64(&calls, 1)
		return "ok", nil
	}, delays)
	assert.NilError(t, err)
	assert.Equal(t, resp, "ok")
	assert.Assert(t, atomic.LoadInt64(&calls) == 1)
}

func TestHedgeAllTimeout(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var calls int64
	delays := []time.Duration{time.Duration(0), time.Second}
	_, err := hedge(ctx, func(ctx context.Context) (interface{}, error) {
		atomic.AddInt64(&calls, 1)
		time := time.NewTimer(time.Minute)
		select {
		case <-time.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return "ok", nil
	}, delays)
	assert.Error(t, err, context.DeadlineExceeded.Error())
	assert.Assert(t, atomic.LoadInt64(&calls) == 2)
	assert.Assert(t, time.Since(start) < time.Minute)
}

func TestFirstError(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var calls int64
	errors := []error{errors.New("Fake error"), nil}
	values := []string{"first", "second"}
	delays := []time.Duration{time.Duration(0), 2 * time.Second}
	value, err := hedge(ctx, func(ctx context.Context) (interface{}, error) {
		v := atomic.AddInt64(&calls, 1) - 1
		return values[v], errors[v]
	}, delays)

	assert.NilError(t, err)
	assert.Equal(t, value, "second")
	assert.Assert(t, time.Since(start) > time.Second)
	assert.Assert(t, atomic.LoadInt64(&calls) == 2)
}
