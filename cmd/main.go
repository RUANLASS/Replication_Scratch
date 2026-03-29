package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"
	"wal_scratch/follower"
	"wal_scratch/healthcheck"
	"wal_scratch/leader"
	"wal_scratch/router"
)

func main() {
	role := flag.String("role", "leader", "leader or follower")
	port := flag.String("port", "8080", "port to listen on")
	followers := flag.String("followers", "", "comma-separated follower URLs")
	leaderURL := flag.String("leader", "http://localhost:8080", "leader URL (follower only)")
	flag.Parse()

	mux := http.NewServeMux()

	switch *role {
	case "leader":
		s, err := leader.NewServer("leader.log")
		if err != nil {
			log.Fatalf("failed to start leader: %v", err)
		}
		s.RegisterRoutes(mux)
		log.Printf("leader listening on :%s", *port)

	case "follower":
		f, err := follower.NewFollower("follower.log", *leaderURL)
		if err != nil {
			log.Fatalf("failed to start follower: %v", err)
		}
		f.Start(5 * time.Second)
		f.RegisterRoutes(mux)
		log.Printf("follower listening on :%s (leader: %s)", *port, *leaderURL)
	case "router":
		followerURLs := strings.Split(*followers, ",")
		ro := router.NewRouter(*leaderURL, followerURLs, 30*time.Second)
		ro.RegisterRoutes(mux)

		hc := healthcheck.NewHealthChecker(ro, followerURLs, 5*time.Second, 3)
		hc.Start()
		log.Printf("router listening on :%s with health checking enabled", *port)
	}

	log.Fatal(http.ListenAndServe(":"+*port, mux))
}
