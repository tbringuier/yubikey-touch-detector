package notifier

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
	log "github.com/sirupsen/logrus"
)

const (
	sniIface = "org.kde.StatusNotifierItem"
	sniPath  = dbus.ObjectPath("/StatusNotifierItem")

	sniWatcherIface = "org.kde.StatusNotifierWatcher"
	sniWatcherPath  = dbus.ObjectPath("/StatusNotifierWatcher")

	sniStatusActive    = "Active"
	sniStatusAttention = "NeedsAttention"

	// Four visual states:
	//   security-medium   = amber padlock  → YubiKey not plugged in
	//   security-high     = green padlock  → YubiKey connected and idle
	//   security-low      = red padlock    → touch required (blinks as AttentionIconName)
	//   appointment-soon  = clock          → touch cached (15s countdown after touch)
	sniIconMissing = "security-medium"
	sniIconNormal  = "security-high"
	sniIconAlert   = "security-low"
	sniIconCached  = "appointment-soon"

	// Poll interval for USB presence check.
	yubikeyPollInterval = 2 * time.Second

	// Yubico USB vendor ID (decimal: 4176).
	yubicoVendorID = "1050"

	// Duration YubiKey caches a touch (matches YubiKey firmware default).
	yubikeyTouchCacheDuration = 15 * time.Second

	// How long to retry SNI watcher registration on startup (taskbar may not be ready yet).
	sniWatcherRetryDuration = 120 * time.Second
	sniWatcherRetryInterval = 2 * time.Second
)

// sniServer holds the D-Bus methods required by the StatusNotifierItem interface.
type sniServer struct{}

func (sniServer) ContextMenu(x, y int32) *dbus.Error         { return nil }
func (sniServer) Activate(x, y int32) *dbus.Error            { return nil }
func (sniServer) SecondaryActivate(x, y int32) *dbus.Error   { return nil }
func (sniServer) Scroll(delta int32, orient string) *dbus.Error { return nil }

// yubikeyPresent returns true if a Yubico USB device is detected in the system.
// It reads idVendor files under /sys/bus/usb/devices/ — no external commands needed.
func yubikeyPresent() bool {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return false
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("/sys/bus/usb/devices", e.Name(), "idVendor"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == yubicoVendorID {
			return true
		}
	}
	return false
}

// watchYubikey polls for USB presence changes and sends true/false on ch
// whenever the connection state changes.
func watchYubikey(ch chan<- bool) {
	ticker := time.NewTicker(yubikeyPollInterval)
	defer ticker.Stop()
	prev := yubikeyPresent()
	ch <- prev // send initial state immediately
	for range ticker.C {
		now := yubikeyPresent()
		if now != prev {
			prev = now
			ch <- now
		}
	}
}

// checkYkmanCachedPolicy runs "ykman openpgp info" and returns true if any
// key slot uses a "Cached" touch policy. Returns false silently if ykman is
// not installed or if the command fails for any reason.
func checkYkmanCachedPolicy() bool {
	out, err := exec.Command("ykman", "openpgp", "info").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Touch policy: Cached")
}

// SetupTrayNotifier registers a persistent system-tray icon using the
// StatusNotifierItem (SNI) D-Bus protocol.
//
// Four visual states:
//
//	amber (security-medium)   → YubiKey not plugged in
//	green (security-high)     → YubiKey connected and idle
//	red blinking              → YubiKey waiting for a touch
//	  (Status=NeedsAttention, KDE blinks between IconName and AttentionIconName)
//	clock (appointment-soon)  → touch recently completed, cached for 15 s
//	  (only shown when ykman reports "Touch policy: Cached")
//
// Compatibility:
//   - KDE Plasma (Bazzite): works out of the box.
//   - GNOME (Aurora): requires the "AppIndicator and KStatusNotifierItem Support"
//     extension. install.sh handles this automatically.
func SetupTrayNotifier(notifiers *sync.Map) {
	touch := make(chan Message, 10)
	notifiers.Store("notifier/tray", touch)

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Error("Tray: cannot connect to D-Bus session bus: ", err)
		return
	}
	defer conn.Close()

	serviceName := fmt.Sprintf("%s-%d-1", sniIface, os.Getpid())
	reply, err := conn.RequestName(serviceName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		log.Error("Tray: cannot claim D-Bus name '", serviceName, "': ", err)
		return
	}

	s := sniServer{}
	if err := conn.Export(s, sniPath, sniIface); err != nil {
		log.Error("Tray: cannot export SNI interface: ", err)
		return
	}

	propsSpec := map[string]map[string]*prop.Prop{
		sniIface: {
			"Category": {Value: "Hardware", Writable: false, Emit: prop.EmitFalse},
			"Id":       {Value: "yubikey-touch-detector", Writable: false, Emit: prop.EmitFalse},
			"Title":    {Value: "YubiKey Touch Detector", Writable: false, Emit: prop.EmitFalse},

			// Status drives KDE blinking: "Active" | "NeedsAttention"
			"Status": {Value: sniStatusActive, Writable: true, Emit: prop.EmitTrue},

			// IconName is updated dynamically to reflect connection/idle/touch/cache state.
			"IconName": {Value: sniIconMissing, Writable: true, Emit: prop.EmitTrue},

			// Shown (alternating with IconName) when Status = NeedsAttention.
			"AttentionIconName": {Value: sniIconAlert, Writable: false, Emit: prop.EmitFalse},

			"OverlayIconName":    {Value: "", Writable: false, Emit: prop.EmitFalse},
			"AttentionMovieName": {Value: "", Writable: false, Emit: prop.EmitFalse},
			"ItemIsMenu":         {Value: false, Writable: false, Emit: prop.EmitFalse},
			"WindowId":           {Value: int32(0), Writable: false, Emit: prop.EmitFalse},
		},
	}

	props, err := prop.Export(conn, sniPath, propsSpec)
	if err != nil {
		log.Error("Tray: cannot export SNI properties: ", err)
		return
	}

	node := &introspect.Node{
		Name: string(sniPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name:       sniIface,
				Methods:    introspect.Methods(s),
				Properties: props.Introspection(sniIface),
				Signals: []introspect.Signal{
					{Name: "NewTitle"},
					{Name: "NewIcon"},
					{Name: "NewAttentionIcon"},
					{Name: "NewOverlayIcon"},
					{Name: "NewToolTip"},
					{Name: "NewStatus", Args: []introspect.Arg{
						{Name: "status", Type: "s"},
					}},
				},
			},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), sniPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		log.Error("Tray: cannot export introspection: ", err)
		return
	}

	// Register with the StatusNotifierWatcher in a retry loop.
	// On system startup the taskbar (and its watcher) may not be ready for
	// several seconds; we keep trying for up to sniWatcherRetryDuration.
	watcher := conn.Object(sniWatcherIface, sniWatcherPath)
	go func() {
		deadline := time.Now().Add(sniWatcherRetryDuration)
		for time.Now().Before(deadline) {
			if err := watcher.Call(sniWatcherIface+".RegisterStatusNotifierItem", 0, serviceName).Err; err == nil {
				log.Debug("Tray: registered with StatusNotifierWatcher as ", serviceName)
				return
			}
			log.Debug("Tray: watcher not ready yet, retrying in ", sniWatcherRetryInterval)
			time.Sleep(sniWatcherRetryInterval)
		}
		log.Warn("Tray: StatusNotifierWatcher not available after ", sniWatcherRetryDuration)
		log.Warn("Tray: on GNOME, install the 'AppIndicator and KStatusNotifierItem Support' extension")
	}()

	setStatus := func(status string) {
		if dbusErr := props.Set(sniIface, "Status", dbus.MakeVariant(status)); dbusErr != nil {
			log.Warn("Tray: cannot update Status: ", dbusErr)
			return
		}
		if err := conn.Emit(sniPath, sniIface+".NewStatus", status); err != nil {
			log.Warn("Tray: cannot emit NewStatus: ", err)
		}
		log.Debug("Tray: status → ", status)
	}

	setIcon := func(icon string) {
		if dbusErr := props.Set(sniIface, "IconName", dbus.MakeVariant(icon)); dbusErr != nil {
			log.Warn("Tray: cannot update IconName: ", dbusErr)
			return
		}
		if err := conn.Emit(sniPath, sniIface+".NewIcon"); err != nil {
			log.Warn("Tray: cannot emit NewIcon: ", err)
		}
		log.Debug("Tray: icon → ", icon)
	}

	// ykmanResultCh receives the result of checkYkmanCachedPolicy() each time
	// the YubiKey is connected (non-blocking send: old results are dropped).
	ykmanResultCh := make(chan bool, 1)

	// cacheExpiredCh is signalled when the 15-second post-touch cache window ends.
	cacheExpiredCh := make(chan struct{}, 1)

	// Start USB presence watcher.
	keyPresenceCh := make(chan bool, 1)
	go watchYubikey(keyPresenceCh)

	activeTouchWaits := 0
	keyPresent := false
	hasCachedPolicy := false
	var cacheTimer *time.Timer

	for {
		select {
		case present := <-keyPresenceCh:
			keyPresent = present
			if !present {
				// YubiKey unplugged: cancel all pending state.
				hasCachedPolicy = false
				if cacheTimer != nil {
					cacheTimer.Stop()
					cacheTimer = nil
				}
				activeTouchWaits = 0
				setStatus(sniStatusActive)
				setIcon(sniIconMissing)
				log.Debug("Tray: YubiKey disconnected")
			} else {
				// YubiKey plugged in: switch to green idle icon and check touch policy.
				setIcon(sniIconNormal)
				log.Debug("Tray: YubiKey connected")
				go func() {
					// Give the device a moment to initialize before querying ykman.
					time.Sleep(500 * time.Millisecond)
					cached := checkYkmanCachedPolicy()
					select {
					case ykmanResultCh <- cached:
					default: // drop if a previous result is still pending
					}
				}()
			}

		case cached := <-ykmanResultCh:
			hasCachedPolicy = cached
			if cached {
				log.Debug("Tray: YubiKey has Cached touch policy — hourglass icon enabled")
			}

		case <-cacheExpiredCh:
			// 15-second touch-cache window expired; revert to idle icon.
			cacheTimer = nil
			if activeTouchWaits == 0 && keyPresent {
				setIcon(sniIconNormal)
				log.Debug("Tray: touch cache expired → idle")
			}

		case value := <-touch:
			// Track whether the touch that just completed was GPG-based.
			// Only OpenPGP (and PIV) support a "Cached" touch policy on the YubiKey;
			// FIDO/U2F does not cache touches, so the hourglass icon must not appear
			// after a U2F or HMAC touch even when ykman reports Cached policy.
			wasGPGOff := value == GPG_OFF
			switch value {
			case GPG_ON, U2F_ON, HMAC_ON:
				activeTouchWaits++
				// Cancel any running cache countdown when a new touch is requested.
				if cacheTimer != nil {
					cacheTimer.Stop()
					cacheTimer = nil
				}
			case GPG_OFF, U2F_OFF, HMAC_OFF:
				if activeTouchWaits > 0 {
					activeTouchWaits--
				}
			}

			if activeTouchWaits > 0 {
				// NeedsAttention causes KDE to blink between IconName and AttentionIconName.
				setStatus(sniStatusAttention)
			} else {
				setStatus(sniStatusActive)
				if !keyPresent {
					setIcon(sniIconMissing)
				} else if hasCachedPolicy && wasGPGOff {
					// Show hourglass icon for 15 seconds to indicate the GPG touch is cached.
					// Not shown for U2F/HMAC: FIDO does not implement touch caching.
					setIcon(sniIconCached)
					if cacheTimer != nil {
						cacheTimer.Stop()
					}
					cacheTimer = time.AfterFunc(yubikeyTouchCacheDuration, func() {
						select {
						case cacheExpiredCh <- struct{}{}:
						default:
						}
					})
					log.Debug("Tray: GPG touch cached → hourglass for ", yubikeyTouchCacheDuration)
				} else {
					setIcon(sniIconNormal)
				}
			}
		}
	}
}
