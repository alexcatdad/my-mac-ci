#include "MMCLoginKeychain.h"

// These APIs are required for the existing file-based login Keychain item.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

OSStatus MMCKeychainGetUserInteractionAllowed(Boolean *allowed) {
    return SecKeychainGetUserInteractionAllowed(allowed);
}

OSStatus MMCKeychainSetUserInteractionAllowed(Boolean allowed) {
    return SecKeychainSetUserInteractionAllowed(allowed);
}

OSStatus MMCKeychainOpen(const char *path, SecKeychainRef *keychain) {
    return SecKeychainOpen(path, keychain);
}

OSStatus MMCKeychainGetStatus(SecKeychainRef keychain, SecKeychainStatus *status) {
    return SecKeychainGetStatus(keychain, status);
}

#pragma clang diagnostic pop
