//go:build darwin && cgo

package isolation

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <SystemConfiguration/SystemConfiguration.h>

static SCDynamicStoreRef watchNetwork(void) {
	SCDynamicStoreRef store = SCDynamicStoreCreate(NULL, CFSTR("MMC isolation monitor"), NULL, NULL);
	if (!store) return NULL;
	const void *patterns[] = { CFSTR("^(State|Setup):/Network/.*") };
	CFArrayRef keys = CFArrayCreate(NULL, patterns, 1, &kCFTypeArrayCallBacks);
	Boolean ok = keys && SCDynamicStoreSetNotificationKeys(store, NULL, keys);
	if (keys) CFRelease(keys);
	if (ok) return store;
	CFRelease(store);
	return NULL;
}

static Boolean networkKey(CFArrayRef keys, CFIndex index, char *buf, CFIndex size) {
 return CFStringGetCString(CFArrayGetValueAtIndex(keys,index),buf,size,kCFStringEncodingUTF8);
}
*/
import "C"

import (
	"net"
	"syscall"
)

func observeHost() (func(map[string]bool) (bool, error), func(), error) {
	store := C.watchNetwork()
	if store == 0 {
		return nil, nil, errHostEvents
	}
	var lastWake string
	var before map[string]int
	changed := func(owned map[string]bool) (bool, error) {
		changes := C.SCDynamicStoreCopyNotifiedKeys(store)
		if changes == 0 {
			return false, errHostEvents
		}
		defer C.CFRelease(C.CFTypeRef(changes))
		var keys []string
		var buf [1024]C.char
		for i := C.CFIndex(0); i < C.CFArrayGetCount(changes); i++ {
			if C.networkKey(changes, i, &buf[0], C.CFIndex(len(buf))) == 0 {
				return false, errHostEvents
			}
			keys = append(keys, C.GoString(&buf[0]))
		}
		ifaces, err := net.Interfaces()
		if err != nil {
			return false, errHostEvents
		}
		after := make(map[string]int, len(ifaces))
		for _, iface := range ifaces {
			after[iface.Name] = iface.Index
		}
		network := probeChanges(keys, before, after, owned)
		before = after
		wake, err := syscall.Sysctl("kern.waketime")
		if err != nil {
			return false, errHostEvents
		}
		newWake := wake != lastWake
		lastWake = wake
		return network || newWake, nil
	}
	return changed, func() { C.CFRelease(C.CFTypeRef(store)) }, nil
}
