// Command seed fills a running stack with a demo dataset.
//
// It drives the HTTP API rather than the Temporal client, so seeding exercises
// exactly the path a user does -- including the rollover retry and the error
// mapping. If the API is wrong, the seed notices.
//
// The customers deliberately mirror cmd/mockapi's fixtures, name for name, so
// the UI can be pointed at either backend and show the same people. That makes
// "does this behave the same against the real thing?" a question you answer by
// changing a port rather than by reading two lists.
//
//	make seed          # needs `make up`, `make worker`, `make api`
//	make seed FRESH=1  # deactivate existing customers first
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// customer is one seeded record. Points are reached by repeated adds, since
// that is the only way points ever enter the system -- there is no back door,
// which is rather the point of the design.
type customer struct {
	id, name, email string
	// adds is the sequence of point-adds to apply, in order. Chosen so tier
	// crossings and continue-as-new boundaries land where the demo wants them.
	adds []int
	// deactivate closes the customer at the end.
	deactivate bool
	why        string
}

var seedSet = []customer{
	{
		id: "ada", name: "Ada Lovelace", email: "ada@example.com",
		adds: []int{120, 200, 180, 60, 40, 20, 20},
		why:  "ordinary active customer, gold, a few generations in",
	},
	{
		id: "grace", name: "Grace Hopper", email: "grace@example.com",
		adds: []int{500, 500, 250, 250},
		why:  "top tier: nextTierAt is 0, which the progress bar has to survive",
	},
	{
		id: "newbie", name: "Newly Enrolled", email: "new@example.com",
		why: "enrolled, never earned -- the empty-timeline case",
	},
	{
		id: "departed", name: "Gone Away", email: "gone@example.com",
		adds: []int{100, 100, 100, 10}, deactivate: true,
		why: "deactivated: no add-points form, and a departure notification",
	},
	// Filler, so an unfiltered list exceeds ListLimit and the
	// "Showing 5 of 9 -- filter to find additional results" notice renders.
	{id: "alan", name: "Alan Turing", email: "alan@example.com", adds: []int{300, 180}, why: "filler"},
	{id: "barbara", name: "Barbara Liskov", email: "barbara@example.com", adds: []int{400, 320}, why: "filler"},
	{id: "edsger", name: "Edsger Dijkstra", email: "edsger@example.com", adds: []int{600, 600}, why: "filler"},
	{id: "katherine", name: "Katherine Johnson", email: "katherine@example.com", adds: []int{95}, why: "filler"},
}

// cappedAdds takes a customer to 99,960 points: high enough that any add over
// 40 gets a *handler* rejection, which -- unlike a validator rejection -- leaves
// an audit row. Reaching it organically takes 100 adds, which is exactly why the
// UI's rejection rendering goes untested without a seeded customer like this.
//
// It also produces ~33 generations, so `make reap WF=customer-capped` gives an
// audit log truncated to a handful of rows out of hundreds.
func cappedAdds() []int {
	adds := make([]int, 0, 100)
	for i := 0; i < 99; i++ {
		adds = append(adds, 1000)
	}
	return append(adds, 960)
}

func main() {
	base := env("API_BASE", "http://localhost:8081")
	fresh := os.Getenv("FRESH") != ""

	if err := ping(base); err != nil {
		log.Fatalf("no API at %s: %v\nis `make api` running (and `make up` and `make worker`)?", base, err)
	}

	set := append([]customer{}, seedSet...)
	set = append(set, customer{
		id: "capped", name: "Max Capacity", email: "max@example.com",
		adds: cappedAdds(),
		why:  "just under the points cap, so handler rejections are reachable",
	})

	start := time.Now()
	for _, c := range set {
		if fresh {
			_ = do(http.MethodDelete, base+"/api/customers/"+c.id, nil, nil)
		}
		if err := seed(base, c); err != nil {
			log.Printf("  %-10s SKIPPED: %v", c.id, err)
			continue
		}
		fmt.Printf("  %-10s %-4d adds  %s\n", c.id, len(c.adds), c.why)
	}

	fmt.Printf("\nseeded %d customers in %s\n", len(set), time.Since(start).Round(time.Millisecond))
	fmt.Printf("\n  %s/api/customers\n", base)
	fmt.Printf("  make audit ID=ada\n")
	fmt.Printf("  make reap WF=customer-capped     # then `make audit ID=capped` for a truncated log\n")
}

func seed(base string, c customer) error {
	err := do(http.MethodPost, base+"/api/customers", map[string]string{
		"customerId": c.id, "name": c.name, "email": c.email,
	}, nil)
	if err != nil {
		return err
	}

	for i, amount := range c.adds {
		body := map[string]any{
			"amount": amount,
			"reason": fmt.Sprintf("seed purchase %d", i+1),
			// A fresh idempotency key per add, as the UI sends. Deterministic
			// here so a re-run is traceable rather than random.
			"requestId": fmt.Sprintf("seed-%s-%03d", c.id, i+1),
		}
		if err := do(http.MethodPost, base+"/api/customers/"+c.id+"/points", body, nil); err != nil {
			return fmt.Errorf("add %d: %w", i+1, err)
		}
	}

	if c.deactivate {
		return do(http.MethodDelete, base+"/api/customers/"+c.id, nil, nil)
	}
	return nil
}

func ping(base string) error {
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

// do sends a request and turns a non-2xx into the API's own error message,
// which is more useful than a status code -- and demonstrates that the error
// contract is usable by something other than the UI.
func do(method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		raw, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			return fmt.Errorf("%s (%s)", e.Error.Message, e.Error.Code)
		}
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
