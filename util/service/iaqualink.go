package service

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/evcc-io/evcc/server/service"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/tekkamanendless/iaqualink"
)

func init() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", iaqualinkDevices)
	service.Register("iaqualink", mux)
}

// iaqualinkDevices validates IAquaLink cloud credentials and returns device identifiers for the config UI.
// Query: email, user (optional alternative to email), password (required).
func iaqualinkDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	login := strings.TrimSpace(q.Get("email"))
	if login == "" {
		login = strings.TrimSpace(q.Get("user"))
	}
	password := q.Get("password")
	if login == "" || password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "email or user and password are required",
		})
		return
	}

	log := util.NewLogger("iaqualink").Redact(login, password)
	client := &iaqualink.Client{
		Client: request.NewClient(log),
	}

	if _, err := client.Login(login, password); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "IAquaLink login failed: " + err.Error(),
		})
		return
	}

	devices, err := client.ListDevices()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "IAquaLink device list failed: " + err.Error(),
		})
		return
	}

	out := make([]string, 0, len(devices))
	for _, d := range devices {
		if d.SerialNumber != "" {
			out = append(out, d.SerialNumber)
			continue
		}
		if d.Name != "" {
			out = append(out, d.Name)
			continue
		}
		out = append(out, strconv.Itoa(d.ID))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
