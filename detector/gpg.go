package detector

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/proglottis/gpgme"
	"github.com/rjeczalik/notify"
	log "github.com/sirupsen/logrus"

	"github.com/maximbaz/yubikey-touch-detector/notifier"
)

const (
	gpgUSBPollInterval   = 2 * time.Second
	gpgUSBTriggerWindow  = 30 * time.Second
	gpgUSBTriggerPeriod  = 500 * time.Millisecond
	gpgUSBInitialDelay   = 1500 * time.Millisecond
	yubicoVendorIDGPG    = "1050"
)

func yubiKeyPresentGPG() bool {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return false
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("/sys/bus/usb/devices", e.Name(), "idVendor"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == yubicoVendorIDGPG {
			return true
		}
	}
	return false
}

// WatchUSBForGPGCheck polls for YubiKey USB reconnect events and fires
// requestGPGCheck repeatedly for gpgUSBTriggerWindow after each reconnect.
// This compensates for gpg-agent caching shadowed-key file handles after
// the first use, which prevents inotify InOpen events from firing on
// subsequent operations after a plug/unplug cycle.
func WatchUSBForGPGCheck(requestGPGCheck chan bool) {
	ticker := time.NewTicker(gpgUSBPollInterval)
	defer ticker.Stop()

	prev := yubiKeyPresentGPG()
	for range ticker.C {
		now := yubiKeyPresentGPG()
		if now && !prev {
			// Key reconnected: give scdaemon time to reinitialize,
			// then flood requestGPGCheck for the duration of the touch window.
			log.Debug("GPG/USB: YubiKey reconnected — starting trigger window")
			go func() {
				time.Sleep(gpgUSBInitialDelay)
				deadline := time.Now().Add(gpgUSBTriggerWindow)
				t := time.NewTicker(gpgUSBTriggerPeriod)
				defer t.Stop()
				for time.Now().Before(deadline) {
					<-t.C
					select {
					case requestGPGCheck <- true:
					default:
					}
				}
				log.Debug("GPG/USB: trigger window expired")
			}()
		}
		prev = now
	}
}

// WatchGPG watches for hints that YubiKey is maybe waiting for a touch on a GPG request
func WatchGPG(filesToWatch []string, requestGPGCheck chan bool) {
	// No need for a buffered channel,
	// we are interested only in the first event, it's ok to skip all subsequent ones
	events := make(chan notify.EventInfo)

	initWatcher := func() {
		for _, file := range filesToWatch {
			if err := notify.Watch(file, events, notify.InOpen, notify.InDeleteSelf, notify.InMoveSelf); err != nil {
				log.Errorf("Failed to establish a watch on GPG file '%s': %v\n", file, err)
				return
			}
			log.Debugf("GPG watcher is watching '%s'...\n", file)
		}
	}

	initWatcher()
	defer notify.Stop(events)

	for event := range events {
		switch event.Event() {
		case notify.InOpen:
			select {
			case requestGPGCheck <- true:
			default:
			}
		default:
			log.Debugf("GPG received file event '%+v', recreating the watcher.", event.Event())
			notify.Stop(events)
			time.Sleep(5 * time.Second)
			initWatcher()
		}
	}
}

// CheckGPGOnRequest checks whether YubiKey is actually waiting for a touch on a GPG request
func CheckGPGOnRequest(requestGPGCheck chan bool, notifiers *sync.Map, ctx *gpgme.Context) {
	sendToAll := func(msg notifier.Message) {
		notifiers.Range(func(_, v interface{}) bool {
			v.(chan notifier.Message) <- msg
			return true
		})
	}

	// probe sends a LEARN command to gpg-agent and fires GPG_ON/OFF based on the result.
	// If suspectTouch is true, a shorter 100ms timeout is used (for the follow-up probe
	// after PIN entry, where a touch is expected to follow immediately).
	// Returns true if PINENTRY_LAUNCHED was seen (meaning a PIN dialog appeared).
	probe := func(suspectTouch bool) (pinSeen bool) {
		done := make(chan error, 1)
		trigger := make(chan struct{}, 1)

		timeout := 400 * time.Millisecond
		if suspectTouch {
			timeout = 100 * time.Millisecond
		}

		go func() {
			select {
			case <-trigger:
				// Pinentry was launched: show notification immediately, before timeout
			case <-time.After(timeout):
				// Timed out waiting: assume a touch or long operation is in progress
			case err := <-done:
				// LEARN returned before any trigger: card was ready, no touch needed
				if err != nil {
					log.Debugf("GPG probe returned early: %v", err)
				}
				return
			}
			sendToAll(notifier.GPG_ON)
			err := <-done
			if err != nil {
				log.Errorf("Agent returned an error: %v", err)
			}
			sendToAll(notifier.GPG_OFF)
		}()

		err := ctx.AssuanSend("LEARN", nil, nil, func(status, args string) error {
			log.Debugf("AssuanSend/status: %v, %v", status, args)
			// PINENTRY_LAUNCHED means gpg-agent launched a PIN or confirmation dialog.
			// Fire the notification immediately instead of waiting for the 400ms timeout.
			if strings.HasPrefix(status, "PINENTRY_LAUNCHED") {
				pinSeen = true
				select {
				case trigger <- struct{}{}:
				default:
				}
			}
			return nil
		})
		done <- err
		return pinSeen
	}

	for range requestGPGCheck {
		time.Sleep(200 * time.Millisecond) // wait for GPG to start talking with scdaemon
		pinSeen := probe(false)
		if pinSeen {
			// After PIN entry the actual crypto operation (sign/auth/decrypt) typically
			// needs a touch. Send a second probe with a shorter timeout to catch it —
			// scdaemon will queue our LEARN behind the pending operation, causing it to
			// block until the user touches the key.
			probe(true)
		}
	}
}
