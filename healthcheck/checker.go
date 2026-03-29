package healthcheck

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
	"wal_scratch/follower"
	"wal_scratch/router"
)

type HealthChecker struct {
	router           *router.Router
	followerURLs     []string
	checkInterval    time.Duration
	failureThreshold int
	httpClient       *http.Client
	fails            int32
	cancel           context.CancelFunc
	mu               sync.Mutex
}

func NewHealthChecker(ro *router.Router, followerURLs []string, interval time.Duration, threshold int) *HealthChecker {
	healthChecker := HealthChecker{
		router:           ro,
		followerURLs:     followerURLs,
		checkInterval:    interval,
		failureThreshold: threshold,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
		},
		fails: 0,
	}
	return &healthChecker
}

func (hc *HealthChecker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	hc.mu.Lock()
	hc.cancel = cancel
	hc.mu.Unlock()

	ticker := time.NewTicker(hc.checkInterval)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hc.check()
			}
		}
	}()
}

func (hc *HealthChecker) check() {
	leaderURL := hc.router.ReturnleaderURL()
	resp, err := hc.httpClient.Get(leaderURL + "/health")
	if err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		hc.mu.Lock()
		hc.fails = 0
		hc.mu.Unlock()
		return
	}

	if resp != nil {
		resp.Body.Close()
	}

	hc.mu.Lock()
	hc.fails++
	consFails := hc.fails
	threshold := hc.failureThreshold
	hc.mu.Unlock()

	fmt.Printf("Leader health check failed (%d/%d)\n", consFails, threshold)

	if int(consFails) < threshold {
		return // Not yet convinced the leader is truly dead
	}

	// 4. PROMOTE: Threshold reached
	fmt.Println("Leader threshold reached. Initiating failover...")
	hc.promoteFollower()

}

func (hc *HealthChecker) promoteFollower() {
	var bestFollower string
	var maxOffset uint64
	foundCandidate := false
	for _, url := range hc.followerURLs {
		offset, err := follower.FetchAppliedOffset(url)
		if err != nil {
			fmt.Printf("Election: skipping unreachable follower %s: %v\n", url, err)
			continue
		}

		if !foundCandidate || offset > maxOffset {
			maxOffset = offset
			bestFollower = url
			foundCandidate = true
		}
	}
	if !foundCandidate {
		fmt.Println("CRITICAL: No healthy followers found to promote!")
		return
	}
	promoteURL := fmt.Sprintf("%s/promote", bestFollower)
	resp, err := hc.httpClient.Post(promoteURL, "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Printf("FAILED to promote %s: %v\n", bestFollower, err)
		return
	}
	defer resp.Body.Close()
	hc.router.SetLeader(bestFollower)

	fmt.Printf("ELECTION COMPLETE: %s promoted to Leader at offset %d\n", bestFollower, maxOffset)
}
