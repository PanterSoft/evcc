package tasks

import (
	"runtime"
	"strings"
	"time"

	"github.com/evcc-io/evcc/util"
	ping "github.com/prometheus-community/pro-bing"
)

const Ping TaskType = "ping"

func init() {
	registry.Add(Ping, PingHandlerFactory)
}

func PingHandlerFactory(conf map[string]any) (TaskHandler, error) {
	handler := PingHandler{
		Count:   1,
		Timeout: timeout,
	}

	err := util.DecodeOther(conf, &handler)

	return &handler, err
}

type PingHandler struct {
	Count   int
	Timeout time.Duration
}

func (h *PingHandler) Test(log *util.Logger, in ResultDetails) []ResultDetails {
	pinger, err := ping.NewPinger(in.IP)
	if err != nil {
		panic(err)
	}

	if runtime.GOOS == "windows" {
		pinger.Size = 548 // https://github.com/go-ping/ping/issues/168
		pinger.SetPrivileged(true)
	}

	pinger.Count = h.Count
	pinger.Timeout = h.Timeout

	if err = pinger.Run(); err != nil {
		if pingErrPermission(err) {
			log.FATAL.Println("ping:", err)
			log.FATAL.Println("")
			log.FATAL.Println("In order to run evcc in discovery mode, allow unprivileged ICMP:")
			log.FATAL.Println("")
			switch runtime.GOOS {
			case "darwin":
				log.FATAL.Println("	macOS: run evcc with sudo for this scan, or grant the binary permission to send ICMP.")
			case "windows":
				log.FATAL.Println("	Run evcc as Administrator for ICMP, or use SetPrivileged ping.")
			default:
				log.FATAL.Println("	sudo sysctl -w net.ipv4.ping_group_range=\"0 2147483647\"")
			}
			log.FATAL.Fatalln("")
		}
		// Unreachable host, ICMP blocked, or transient error — skip this IP only (do not abort the whole scan).
		log.TRACE.Printf("ping %s: %v", in.IP, err)
		return nil
	}

	stat := pinger.Statistics()

	if stat.PacketsRecv == 0 {
		return nil
	}

	return []ResultDetails{in}
}

func pingErrPermission(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "operation not permitted") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "access denied")
}
