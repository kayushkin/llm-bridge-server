package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestADiscordGatewayTimeoutIsTriedAgainAndAPersistentOneFails(t *testing.T) {
	calls := 0
	failFirst := 1
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= failFirst {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer fake.Close()
	previous := discordAPI
	discordAPI = fake.URL
	defer func() { discordAPI = previous }()
	previousWait := retryWait
	retryWait = 0
	defer func() { retryWait = previousWait }()
	client := &discordClient{token: "t", http: fake.Client()}
	var user discordUser
	if err := client.get("/users/@me", nil, &user); err != nil || user.ID != "1" {
		t.Fatalf("after one timeout: %v %+v", err, user)
	}
	calls, failFirst = 0, 100
	if err := client.get("/users/@me", nil, &user); err == nil {
		t.Fatal("a gateway that keeps timing out must fail the call")
	}
}
