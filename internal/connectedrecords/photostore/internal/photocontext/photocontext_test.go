package photocontext

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDoWaitsOutAStoreThatIsRestarting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	go func() {
		time.Sleep(2 * time.Second)
		restarted, err := net.Listen("tcp", address)
		if err != nil {
			t.Error(err)
			return
		}
		http.Serve(restarted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		}))
	}()
	var answer struct {
		OK bool `json:"ok"`
	}
	client := Client{HTTP: &http.Client{Timeout: 10 * time.Second}}
	if err := client.GetJSON("http://"+address+"/health", nil, &answer); err != nil || !answer.OK {
		t.Fatalf("GET through a restart = %v %+v, want the answer once the store is back", err, answer)
	}
	if status, _, err := client.PostJSON("http://"+address+"/x", map[string]int{"a": 1}); err != nil || status != http.StatusOK {
		t.Fatalf("POST = %d %v", status, err)
	}
}
