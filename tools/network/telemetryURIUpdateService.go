package network

import (
	"time"

	"github.com/vincentbdb/go-algorand/config"
	"github.com/vincentbdb/go-algorand/logging"
	"github.com/vincentbdb/go-algorand/protocol"
)

// StartTelemetryURIUpdateService starts a go routine which queries SRV records for a telemetry URI every <interval>
func StartTelemetryURIUpdateService(interval time.Duration, cfg config.Local, genesisNetwork protocol.NetworkID, log logging.Logger, abort chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		updateTelemetryURI := func() {
			endpoint := lookupTelemetryEndpoint(cfg, genesisNetwork, log)

			if endpoint != "" && endpoint != log.GetTelemetryURI() {
				log.UpdateTelemetryURI(endpoint)
			}
		}

		// Update telemetry right away, followed by once every <interval>
		updateTelemetryURI()
		for {
			select {
			case <-ticker.C:
				updateTelemetryURI()
			case <-abort:
				return
			}
		}
	}()
}

func lookupTelemetryEndpoint(cfg config.Local, genesisNetwork protocol.NetworkID, log logging.Logger) string {
	bootstrapArray := cfg.DNSBootstrapArray(genesisNetwork)
	bootstrapArray = append(bootstrapArray, "default.algodev.network")
	for _, bootstrapID := range bootstrapArray {
		addrs, err := ReadFromSRV("telemetry", bootstrapID, cfg.FallbackDNSResolverAddress)
		if err != nil {
			log.Warn("An issue occurred reading telemetry entry for: %s", bootstrapID)
		} else if len(addrs) == 0 {
			log.Warn("No telemetry entry for: %s", bootstrapID)
		} else {
			return addrs[0]
		}
	}

	log.Warn("No telemetry endpoint was found.")
	return ""
}
