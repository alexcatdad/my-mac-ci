#pragma once

#include <Security/Security.h>

OSStatus MMCKeychainGetUserInteractionAllowed(Boolean * _Nonnull allowed);
OSStatus MMCKeychainSetUserInteractionAllowed(Boolean allowed);
OSStatus MMCKeychainOpen(const char * _Nonnull path, SecKeychainRef _Nullable * _Nonnull CF_RETURNS_RETAINED keychain);
OSStatus MMCKeychainGetStatus(SecKeychainRef _Nonnull keychain, SecKeychainStatus * _Nonnull status);
