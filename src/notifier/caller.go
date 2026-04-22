package notifier

import "sync"

// callerNames stores the detected calling application for each active touch source.
// Keys are "GPG", "U2F", "HMAC". Values are human-readable process names.
// Written by detectors before sending *_ON messages; read by notifiers when
// building notification text.
var callerNames sync.Map

// SetCallerName records the detected caller for a touch source.
// Must be called before sending the corresponding *_ON message so that the
// notifier sees the name when it processes the message.
func SetCallerName(source, name string) {
	callerNames.Store(source, name)
}

// GetCallerName returns the stored caller name for a touch source, or "" if unknown.
func GetCallerName(source string) string {
	if v, ok := callerNames.Load(source); ok {
		return v.(string)
	}
	return ""
}

// ClearCallerName removes the stored caller name for a touch source.
// Call this after sending the corresponding *_OFF message.
func ClearCallerName(source string) {
	callerNames.Delete(source)
}
