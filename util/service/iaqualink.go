package service

import (
	"errors"
	"net/http"

	iaqualink "github.com/PanterSoft/iAqualink_go"
	"github.com/evcc-io/evcc/server/service"
	"github.com/samber/lo"
)

func init() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", iaqualinkDevices)
	service.Register("iaqualink", mux)
}

// iaqualinkDevices returns the serial numbers of the heat pumps of an iAqualink account
func iaqualinkDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	user, password := q.Get("user"), q.Get("password")

	if user == "" || password == "" {
		jsonError(w, http.StatusBadRequest, errors.New("user and password are required"))
		return
	}

	client, err := iaqualink.NewClient(r.Context(), user, password)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err)
		return
	}

	devices, err := client.ListDevices(r.Context())
	if err != nil {
		jsonError(w, http.StatusBadGateway, err)
		return
	}

	jsonWrite(w, lo.FilterMap(devices, func(d iaqualink.DeviceInfo, _ int) (string, bool) {
		return d.SerialNumber, d.DeviceType == iaqualink.DeviceTypeZS500
	}))
}
