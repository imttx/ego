package ali

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/golang/protobuf/proto"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/gotomicro/ego/core/eapp"
	"github.com/gotomicro/ego/core/elog/ali/pb"
	"github.com/gotomicro/ego/core/emetric"
	"github.com/gotomicro/ego/core/util/xcast"
)

const (
	// entryChanSize sets the logs size
	entryChanSize int = 4096
	// observe interval
	observeInterval = 5 * time.Second
)

type LogContent = pb.Log_Content

// config is the config for Ali Log
type config struct {
	encoder             zapcore.Encoder
	project             string
	endpoint            string
	accessKeyID         string
	accessKeySecret     string
	logstore            string
	topics              []string
	source              string
	flushSize           int
	flushBufferSize     int32
	flushBufferInterval time.Duration
	levelEnabler        zapcore.LevelEnabler
	apiBulkSize         int
	apiTimeout          time.Duration
	apiRetryCount       int
	apiRetryWaitTime    time.Duration
	apiRetryMaxWaitTime time.Duration
	fallbackCore        zapcore.Core
}

// writer implements LoggerInterface.
// Writes messages in keep-live tcp connection.
type writer struct {
	fallbackCore zapcore.Core
	store        *LogStore
	ch           chan *pb.Log
	curBufSize   *int32
	cancel       context.CancelFunc
	config
}

func retryCondition(r *resty.Response, err error) bool {
	code := r.StatusCode()
	if code == 500 || code == 502 || code == 503 {
		return true
	}
	return false
}

// newWriter creates a new ali writer
func newWriter(c config) (*writer, error) {
	entryChanSize := entryChanSize
	if c.apiBulkSize >= entryChanSize {
		c.apiBulkSize = entryChanSize
	}
	w := &writer{config: c, ch: make(chan *pb.Log, entryChanSize), curBufSize: new(int32)}
	p := &LogProject{
		name:            w.project,
		endpoint:        w.endpoint,
		accessKeyID:     w.accessKeyID,
		accessKeySecret: w.accessKeySecret,
	}
	p.initHost()
	p.cli = resty.New().
		SetDebug(eapp.IsDevelopmentMode()).
		SetHostURL(p.host).
		SetTimeout(c.apiTimeout).
		SetRetryCount(c.apiRetryCount).
		SetRetryWaitTime(c.apiRetryWaitTime).
		SetRetryMaxWaitTime(c.apiRetryMaxWaitTime).
		AddRetryCondition(retryCondition)
	store, err := p.GetLogStore(w.logstore)
	if err != nil {
		return nil, fmt.Errorf("getlogstroe fail,%w", err)
	}
	w.store = store
	w.fallbackCore = c.fallbackCore
	w.sync()
	w.observe()
	return w, nil
}

func genLog(fields map[string]interface{}) *pb.Log {
	l := &pb.Log{
		Time:     proto.Uint32(uint32(time.Now().Unix())),
		Contents: make([]*LogContent, 0, len(fields)),
	}
	for k, v := range fields {
		l.Contents = append(l.Contents, &LogContent{
			Key:   proto.String(k),
			Value: proto.String(xcast.ToString(v)),
		})
	}
	return l
}

func (w *writer) write(fields map[string]interface{}) (err error) {
	l := genLog(fields)
	// if bufferSize bigger then defaultBufferSize or channel is full, then flush logs
	w.ch <- l
	atomic.AddInt32(w.curBufSize, int32(l.XXX_Size()))
	if atomic.LoadInt32(w.curBufSize) >= w.flushBufferSize || len(w.ch) >= cap(w.ch) {
		err = w.flush()
	}
	return
}

func (w *writer) flush() error {
	entriesChLen := len(w.ch)
	if entriesChLen == 0 {
		return nil
	}

	var waitedEntries = make([]*pb.Log, 0, entriesChLen)
	waitedEntries = append(waitedEntries, <-w.ch)
L1:
	for i := 0; i < entriesChLen; i++ {
		select {
		case l := <-w.ch:
			waitedEntries = append(waitedEntries, l)
		default:
			break L1
		}
	}

	chunks := int(math.Ceil(float64(len(waitedEntries)) / float64(w.apiBulkSize)))
	for i := 0; i < chunks; i++ {
		go func(start int) {
			end := (start + 1) * w.apiBulkSize
			if end > len(waitedEntries) {
				end = len(waitedEntries)
			}
			lg := pb.LogGroup{Logs: waitedEntries[start:end]}
			if e := w.store.PutLogs(&lg); e != nil {
				// if error occurs we put logs with fallbackCore logger
				for _, v := range lg.Logs {
					fields := make([]zapcore.Field, len(v.Contents), len(v.Contents))
					for i, val := range v.Contents {
						fields[i] = zap.String(val.GetKey(), val.GetValue())
					}
					if e := w.fallbackCore.Write(zapcore.Entry{Time: time.Now()}, fields); e != nil {
						log.Println("fallbackCore write fail", e)
					}
				}
			}
		}(i)
	}

	return nil
}

func (w *writer) sync() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	ticker := time.NewTicker(w.flushBufferInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := w.flush(); err != nil {
					log.Printf("writer flush fail, %s\n", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return
}

func (w *writer) observe() {
	go func() {
		for {
			emetric.LibHandleSummary.Observe(float64(len(w.ch)), "elog", "ali_waited_entries")
			time.Sleep(observeInterval)
		}
	}()
}
