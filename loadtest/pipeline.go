package main

import (
	"context"
	"log"
	"runtime"
	"sync/atomic"
	"time"

	generated "github.com/franz101/sqd-go/examples/polymarket/generated"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
)

type Pipeline struct {
	generator            *EventGenerator
	store                *database.Store
	proc                 *generated.Processor
	blockChan            chan []ingestion.CustomLog
	totalTxs             uint64
	totalBlocks          uint64
	backpressureWaitTime int64 // nanoseconds
	consumerProcessTime  int64 // nanoseconds
	queueCapacity        int
}

func NewPipeline(generator *EventGenerator, store *database.Store, queueCapacity int) (*Pipeline, error) {
	proc, err := generated.NewProcessor(false)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		generator:     generator,
		store:         store,
		proc:          proc,
		blockChan:     make(chan []ingestion.CustomLog, queueCapacity),
		queueCapacity: queueCapacity,
	}, nil
}

// Start runs the pipeline load test.
func (p *Pipeline) Start(ctx context.Context, numBlocks uint64, txsPerBlock int, targetTPS int) {
	log.Printf("Starting loadtest pipeline (blocks=%d, txsPerBlock=%d, targetTPS=%d, queueCap=%d)...",
		numBlocks, txsPerBlock, targetTPS, p.queueCapacity)

	startTime := time.Now()

	// 1. Start Reporter goroutine
	doneChan := make(chan struct{})
	go p.reportStatsLoop(doneChan, startTime)

	// 2. Start Consumer
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case logs, ok := <-p.blockChan:
				if !ok {
					return
				}
				t0 := time.Now()
				err := p.proc.Process(ctx, p.store, logs)
				if err != nil {
					log.Printf("Consumer process error: %v", err)
					return
				}
				atomic.AddInt64(&p.consumerProcessTime, int64(time.Since(t0)))
				atomic.AddUint64(&p.totalTxs, uint64(len(logs)))
				atomic.AddUint64(&p.totalBlocks, 1)
			}
		}
	}()

	// 3. Start Producer
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(p.blockChan)

		var blockInterval time.Duration
		if targetTPS > 0 {
			blocksPerSec := float64(targetTPS) / float64(txsPerBlock)
			if blocksPerSec > 0 {
				blockInterval = time.Duration(float64(time.Second) / blocksPerSec)
			}
		}

		for b := uint64(1); b <= numBlocks; b++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			t0 := time.Now()
			logs := p.generator.GenerateBlockLogs(b, txsPerBlock)
			genTime := time.Since(t0)

			// If throttled, maintain target block rate (subtracting generation time)
			if blockInterval > 0 && genTime < blockInterval {
				time.Sleep(blockInterval - genTime)
			}

			// Push to queue and measure backpressure wait time
			pushStart := time.Now()
			p.blockChan <- logs
			atomic.AddInt64(&p.backpressureWaitTime, int64(time.Since(pushStart)))
		}
	}()

	// Wait for producer to finish generating and consumer to drain the queue
	<-producerDone
	<-consumerDone
	close(doneChan)

	totalElapsed := time.Since(startTime)
	log.Printf("Pipeline finished. Processed %d blocks (%d transactions) in %v.",
		p.totalBlocks, p.totalTxs, totalElapsed)
	log.Printf("Overall TPS: %.2f tx/sec", float64(p.totalTxs)/totalElapsed.Seconds())
}

func (p *Pipeline) reportStatsLoop(done chan struct{}, startTime time.Time) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastTxs uint64
	var lastTime time.Time = startTime

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			now := time.Now()
			currentTxs := atomic.LoadUint64(&p.totalTxs)
			currentBlocks := atomic.LoadUint64(&p.totalBlocks)
			waitNanos := atomic.LoadInt64(&p.backpressureWaitTime)
			procNanos := atomic.LoadInt64(&p.consumerProcessTime)

			txsDiff := currentTxs - lastTxs
			timeDiff := now.Sub(lastTime).Seconds()
			lastTxs = currentTxs
			lastTime = now

			tps := float64(txsDiff) / timeDiff
			avgTps := float64(currentTxs) / now.Sub(startTime).Seconds()

			queueSize := len(p.blockChan)
			queueUsage := float64(queueSize) / float64(p.queueCapacity) * 100.0

			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)

			log.Printf("[STATS] Elapsed: %v | Blk: %d | TPS: %.1f (avg %.1f) | Queue: %d/%d (%.1f%%) | BP Wait: %v | Consumer Proc: %v | Alloc: %d MB | Sys: %d MB | GC: %d",
				now.Sub(startTime).Round(time.Second),
				currentBlocks,
				tps,
				avgTps,
				queueSize,
				p.queueCapacity,
				queueUsage,
				time.Duration(waitNanos).Round(time.Millisecond),
				time.Duration(procNanos).Round(time.Millisecond),
				memStats.Alloc/1024/1024,
				memStats.Sys/1024/1024,
				memStats.NumGC,
			)
		}
	}
}
