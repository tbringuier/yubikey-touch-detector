package notifier

import (
	"os"
	"syscall"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
	log "github.com/sirupsen/logrus"
)

const (
	dbusMenuIface = "com.canonical.dbusmenu"
	dbusMenuPath  = dbus.ObjectPath("/StatusNotifierItem/Menu")
)

// dbusMenuItem is a menu entry in the com.canonical.dbusmenu layout format.
// It serialises as the D-Bus struct type "(ia{sv}av)".
type dbusMenuItem struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant // each wraps a dbusMenuItem
}

// dbusMenuItemProps is the per-item return type of GetGroupProperties: "a(ia{sv})".
type dbusMenuItemProps struct {
	ID         int32
	Properties map[string]dbus.Variant
}

// dbusMenuEvent is the per-event input type of EventGroup: "a(isvu)".
type dbusMenuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// dbusMenuServer implements com.canonical.dbusmenu with a static two-item menu:
//
//	YubiKey Touch Detector   (disabled label)
//	────────────────────────  (separator)
//	Quit
type dbusMenuServer struct {
	conn     *dbus.Conn
	revision uint32
}

func (m *dbusMenuServer) GetLayout(parentId, recursionDepth int32, propertyNames []string) (uint32, dbusMenuItem, *dbus.Error) {
	leaf := func(id int32, label string, enabled bool) dbus.Variant {
		return dbus.MakeVariant(dbusMenuItem{
			ID: id,
			Properties: map[string]dbus.Variant{
				"label":   dbus.MakeVariant(label),
				"enabled": dbus.MakeVariant(enabled),
				"visible": dbus.MakeVariant(true),
			},
			Children: []dbus.Variant{},
		})
	}
	sep := func(id int32) dbus.Variant {
		return dbus.MakeVariant(dbusMenuItem{
			ID: id,
			Properties: map[string]dbus.Variant{
				"type":    dbus.MakeVariant("separator"),
				"visible": dbus.MakeVariant(true),
			},
			Children: []dbus.Variant{},
		})
	}

	root := dbusMenuItem{
		ID:         0,
		Properties: map[string]dbus.Variant{},
		Children: []dbus.Variant{
			leaf(1, "YubiKey Touch Detector", false),
			sep(2),
			leaf(3, "Quit", true),
		},
	}
	return m.revision, root, nil
}

func (m *dbusMenuServer) GetGroupProperties(ids []int32, propertyNames []string) ([]dbusMenuItemProps, *dbus.Error) {
	return []dbusMenuItemProps{}, nil
}

func (m *dbusMenuServer) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	return dbus.MakeVariant(""), nil
}

// Event handles item activation. ID 3 is "Quit".
func (m *dbusMenuServer) Event(id int32, eventId string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventId == "clicked" && id == 3 {
		log.Debug("Tray: Quit selected from context menu")
		go func() {
			// Send SIGTERM so the existing exit handler in main.go runs cleanly.
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				os.Exit(0)
			}
		}()
	}
	return nil
}

func (m *dbusMenuServer) EventGroup(events []dbusMenuEvent) ([]int32, *dbus.Error) {
	for _, e := range events {
		m.Event(e.ID, e.EventID, e.Data, e.Timestamp) //nolint:errcheck
	}
	return []int32{}, nil
}

func (m *dbusMenuServer) AboutToShow(id int32) (bool, *dbus.Error) {
	return false, nil
}

func (m *dbusMenuServer) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

// setupDBusMenu exports the dbusmenu interface and its properties on conn,
// returning the D-Bus object path to be stored in the SNI Menu property.
func setupDBusMenu(conn *dbus.Conn) (dbus.ObjectPath, error) {
	server := &dbusMenuServer{conn: conn, revision: 1}

	if err := conn.Export(server, dbusMenuPath, dbusMenuIface); err != nil {
		return "", err
	}

	// Export the mandatory dbusmenu properties (Version=3, etc.).
	propsSpec := map[string]map[string]*prop.Prop{
		dbusMenuIface: {
			"Version":       {Value: uint32(3), Writable: false, Emit: prop.EmitFalse},
			"TextDirection": {Value: "ltr", Writable: false, Emit: prop.EmitFalse},
			"Status":        {Value: "normal", Writable: false, Emit: prop.EmitFalse},
			"IconThemePath": {Value: []string{}, Writable: false, Emit: prop.EmitFalse},
		},
	}
	if _, err := prop.Export(conn, dbusMenuPath, propsSpec); err != nil {
		return "", err
	}

	// Introspection so desktop shells can discover the interface.
	node := &introspect.Node{
		Name: string(dbusMenuPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name:    dbusMenuIface,
				Methods: introspect.Methods(server),
				Signals: []introspect.Signal{
					{Name: "ItemsPropertiesUpdated"},
					{Name: "LayoutUpdated"},
					{Name: "ItemActivationRequested"},
				},
			},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), dbusMenuPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return "", err
	}

	return dbusMenuPath, nil
}
