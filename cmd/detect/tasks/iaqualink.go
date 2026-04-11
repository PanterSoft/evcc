package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/util"
)

const Iaqualink TaskType = "iaqualink"

func init() {
	registry.Add(Iaqualink, IaqualinkHandlerFactory)
}

// IaqualinkHandler probes common IAquaLink local HTTP paths after TCP port 80/443 is open.
type IaqualinkHandler struct {
	PerRequest time.Duration // timeout for each GET (default 400ms)
}

func IaqualinkHandlerFactory(conf map[string]any) (TaskHandler, error) {
	h := IaqualinkHandler{
		PerRequest: 400 * time.Millisecond,
	}
	err := util.DecodeOther(conf, &h)
	return &h, err
}

// Test implements TaskHandler.
func (h *IaqualinkHandler) Test(log *util.Logger, in ResultDetails) []ResultDetails {
	port := in.Port
	if port == 0 {
		port = 80
	}

	schema := "http"
	if port == 443 {
		schema = "https"
	}

	base := fmt.Sprintf("%s://%s:%d", schema, in.IP, port)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	mode, matched, err := charger.ProbeLocalCompatible(ctx, base, log, h.PerRequest)
	if err != nil || mode == 0 {
		return nil
	}

	out := in
	out.Port = port
	out.IaqualinkResult = &IaqualinkResult{
		BaseURL:    base,
		Mode:       mode,
		MatchedURL: matched,
	}

	return []ResultDetails{out}
}
