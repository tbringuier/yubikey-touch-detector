package notifier

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/esiqveland/notify"
	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// SetupLibnotifyNotifier configures a notifier to show all touch requests with libnotify
func SetupLibnotifyNotifier(notifiers *sync.Map) {
	touch := make(chan Message, 10)
	notifiers.Store("notifier/libnotify", touch)

	notification := notify.Notification{
		AppName:       "yubikey-touch-detector",
		AppIcon:       "dialog-error",
		Summary:       "⚠ YubiKey is waiting for a touch!",
		Body:          "<b>Touch your YubiKey now to proceed.</b>",
		ExpireTimeout: notify.ExpireTimeoutNever,
	}
	notification.SetUrgency(notify.UrgencyCritical)
	notification.AddHint(notify.Hint{ID: "category", Variant: dbus.MakeVariant("device")})

	conn, notifier, err := connectDBus(&notification.ReplacesID)
	if err != nil {
		log.Error("Cannot initialize desktop notifications: ", err)
		return
	}
	defer conn.Close()
	defer notifier.Close()

	// activeSources tracks which touch sources are currently asserting a touch
	// request. The map key is the source name ("GPG", "U2F", "HMAC").
	activeSources := make(map[string]bool)

	for {
		value := <-touch

		switch value {
		case GPG_ON:
			activeSources["GPG"] = true
		case GPG_OFF:
			delete(activeSources, "GPG")
		case U2F_ON:
			activeSources["U2F"] = true
		case U2F_OFF:
			delete(activeSources, "U2F")
		case HMAC_ON:
			activeSources["HMAC"] = true
		case HMAC_OFF:
			delete(activeSources, "HMAC")
		}

		if len(activeSources) > 0 {
			notification.Body = buildNotificationBody(activeSources)
			id, err := notifier.SendNotification(notification)
			if err != nil {
				log.Error("Cannot show notification (will reconnect to DBUS): ", err)
				notifier.Close()
				conn.Close()
				conn, notifier, err = connectDBus(&notification.ReplacesID)
				if err != nil {
					log.Error("Failed to reconnect: ", err)
					continue
				}
				id, err = notifier.SendNotification(notification)
				if err != nil {
					log.Error("Cannot show notification after reconnect: ", err)
					continue
				}
			}
			atomic.CompareAndSwapUint32(&notification.ReplacesID, 0, id)
		} else if id := atomic.LoadUint32(&notification.ReplacesID); id != 0 {
			if _, err := notifier.CloseNotification(id); err != nil {
				log.Error("Cannot close notification (will reconnect to DBUS): ", err)
				notifier.Close()
				conn.Close()
				conn, notifier, err = connectDBus(&notification.ReplacesID)
				if err != nil {
					log.Error("Failed to reconnect: ", err)
				}
			}
		}
	}
}

// buildNotificationBody returns the HTML body for the notification.
// When the calling application(s) can be identified, they are named explicitly
// so the user knows which program is requesting the touch. For example:
//
//	"<b>git</b> is requesting your YubiKey touch."
//	"<b>github-desktop, ssh</b> are requesting your YubiKey touch."
//	"<b>Touch your YubiKey now to proceed.</b>" (fallback when caller is unknown)
func buildNotificationBody(activeSources map[string]bool) string {
	var callers []string
	for src := range activeSources {
		if name := GetCallerName(src); name != "" {
			callers = append(callers, name)
		}
	}

	if len(callers) == 0 {
		return "<b>Touch your YubiKey now to proceed.</b>"
	}

	// Deduplicate and sort for a stable output when multiple sources are active
	seen := make(map[string]bool, len(callers))
	unique := callers[:0]
	for _, c := range callers {
		if !seen[c] {
			seen[c] = true
			unique = append(unique, c)
		}
	}
	sort.Strings(unique)

	verb := "is"
	if len(unique) > 1 {
		verb = "are"
	}
	return fmt.Sprintf("<b>%s</b> %s requesting your YubiKey touch.",
		strings.Join(unique, ", "), verb)
}

func connectDBus(replacesID *uint32) (*dbus.Conn, notify.Notifier, error) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "unable to create session bus")
	}

	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, nil, errors.Wrapf(err, "unable to authenticate")
	}

	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, nil, errors.Wrapf(err, "unable get bus name")
	}

	reset := func(msg *notify.NotificationClosedSignal) {
		atomic.CompareAndSwapUint32(replacesID, msg.ID, 0)
	}

	notifier, err := notify.New(
		conn,
		notify.WithOnClosed(reset),
		notify.WithLogger(log.StandardLogger()),
	)
	if err != nil {
		conn.Close()
		return nil, nil, errors.Wrapf(err, "unable to initialize D-Bus notifier interface")
	}

	return conn, notifier, nil
}
