package rate

import (
	"math"
	"sync"
	"time"
)

const RefillInterval = 10 * time.Millisecond

type TokenBucket struct {
	Mu             sync.Mutex
	Tokens         float64
	MaxTokens      float64
	RefillRate     float64
	LastRefillTime time.Time
	quit           chan struct{}
}

func NewTokenBucket(maxTokens float64, refillRate float64) *TokenBucket {
	tb := &TokenBucket{
		Tokens:         maxTokens,
		MaxTokens:      maxTokens,
		RefillRate:     refillRate,
		LastRefillTime: time.Now(),
		quit:           make(chan struct{}),
	}
	go tb.startRefill()
	return tb
}

func (tb *TokenBucket) startRefill() {
	ticker := time.NewTicker(RefillInterval)
	defer ticker.Stop()
	for {
		select {
		case <-tb.quit:
			return
		case <-ticker.C:
			tb.Mu.Lock()
			now := time.Now()
			duration := now.Sub(tb.LastRefillTime).Seconds()
			tokensToAdd := tb.RefillRate * duration
			tb.Tokens = math.Min(tb.Tokens+tokensToAdd, tb.MaxTokens)
			tb.LastRefillTime = now
			tb.Mu.Unlock()
		}
	}
}

func (tb *TokenBucket) Stop() {
	close(tb.quit)
}

func (tb *TokenBucket) Request(tokens float64) bool {
	tb.Mu.Lock()
	defer tb.Mu.Unlock()
	if tokens <= tb.Tokens {
		tb.Tokens -= tokens
		return true
	}
	return false
}

func (tb *TokenBucket) WaitForTokens(tokens float64) {
	for !tb.Request(tokens) {
		tb.Mu.Lock()
		missingTokens := tokens - tb.Tokens
		waitSeconds := missingTokens / tb.RefillRate
		tb.Mu.Unlock()
		waitDuration := time.Duration(math.Max(waitSeconds*float64(time.Second), float64(time.Millisecond)))
		time.Sleep(waitDuration)
	}
}
