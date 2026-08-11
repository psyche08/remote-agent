/*
 * AgentHaloLockedUse.m — Apple Authorization Plug-in for Locked Use.
 *
 * This bundle participates in the macOS screensaver-unlock authorization right.
 * It is the enforcing verifier for the grant contract defined by
 * AgentHaloDesktopCore/Grant.swift; Swift's GrantVerifier is a testable
 * mirror of the checks below, not a substitute for them.
 *
 * Design commitments, in order of importance:
 *
 *  1. ALLOW MEANS UNLOCK. On macOS the screensaver right is a `rule` class
 *     right whose rule list is evaluated k-of-n with k = 1: the first branch
 *     that succeeds authorizes the unlock. This mechanism is one such branch,
 *     and `use-login-window-ui` — the password prompt — is the branch after it.
 *
 *     So Allow here does not mean "I do not object". It means "unlock now,
 *     without a password". This mechanism therefore returns Allow only for a
 *     grant that verified and was consumed, and Deny for everything else,
 *     including every failure path.
 *
 *     An earlier version of this file returned Allow unconditionally, on the
 *     reasoning that a mechanism which never objects cannot lock anyone out.
 *     That reasoning holds for an `evaluate-mechanisms` list, where every
 *     mechanism must pass; it is exactly inverted here. Wired into the rule
 *     list as written, it would have unlocked the Mac for anyone who walked up
 *     to it. It was never actually wired in — the installer inserted into a
 *     `mechanisms` key the right does not have, so the plug-in sat inert — but
 *     the intent was wrong and it is recorded here so it is not reintroduced.
 *
 *     Denying cannot lock anyone out: the login-window branch follows, and a
 *     branch that does not authorize simply lets evaluation continue to it.
 *     The dangerous direction here is permissive, not restrictive.
 *
 *  2. IT NEVER TOUCHES THE PASSWORD. The plugin does not read, request, set,
 *     or log kAuthorizationEnvironmentPassword or any credential. It answers
 *     one question — "may this unlock proceed?" — and learns nothing.
 *
 *  3. FRESHNESS IS OURS, NOT THE GRANT'S. A grant that declares a long life is
 *     rejected outright rather than clamped and honored, so a single leaked or
 *     mis-minted grant can never become a durable skeleton key.
 *
 *  4. SINGLE USE IS ATOMIC. A nonce is consumed with O_CREAT|O_EXCL *before*
 *     allowing. If the ledger write fails for any reason, the plugin does not
 *     allow. There is no path where a grant is honored twice.
 *
 *  5. THIS IS NOT A REMOTE-UNLOCK SERVICE. Nothing here lets another
 *     application or local process unlock the machine. The only thing that can
 *     produce a verifiable grant is the process holding the private key, and
 *     grants live for seconds and are consumed by the unlock they authorize.
 *
 * Build and install with the scripts alongside this file. It is not built by
 * CI: it must be compiled and signed on the target Mac.
 */

#import <Foundation/Foundation.h>
#import <Security/AuthorizationPlugin.h>
#import <Security/AuthorizationTags.h>
#import <Security/AuthSession.h>
#import <sys/file.h>
#import <sys/stat.h>
#import <fcntl.h>
#import <pwd.h>
#import <stdint.h>
#import <string.h>
#import <unistd.h>
#import <errno.h>
#import <limits.h>

/* Paths are compile-time constants under a root-owned directory. Nothing here
 * reads a path supplied by a caller, and the agent (running as the user) can
 * write only the staging file the installer grants it. */
#ifndef AGENTHALO_LOCKED_USE_DIR
#define AGENTHALO_LOCKED_USE_DIR "/Library/Application Support/AgentHalo/locked-use"
#endif

#define AGENTHALO_GRANT_PATH   AGENTHALO_LOCKED_USE_DIR "/grant.json"
#define AGENTHALO_PUBKEY_PATH  AGENTHALO_LOCKED_USE_DIR "/public.key"
#define AGENTHALO_LEDGER_DIR   AGENTHALO_LOCKED_USE_DIR "/consumed"
#define AGENTHALO_DEVICE_PATH  AGENTHALO_LOCKED_USE_DIR "/device_id"
#define AGENTHALO_RECEIPT_PATH AGENTHALO_LOCKED_USE_DIR "/receipt"
#define AGENTHALO_PENDING_RECEIPT_PATH AGENTHALO_LOCKED_USE_DIR "/receipt.pending"
#define AGENTHALO_COMPLETION_RECEIPT_PATH AGENTHALO_LOCKED_USE_DIR "/receipt.complete"

/* Must match AgentHaloDesktopCore/Grant.swift. mac/preflight.sh compares these
 * across the language boundary, because a silent drift here does not fail
 * loudly: the agent mints grants this plugin rejects forever, and the only
 * symptom is a Mac that simply never unlocks. */
static const int      kGrantVersion   = 2;
static const NSTimeInterval kMaxGrantTTL  = 15.0;
static const NSTimeInterval kMaxClockSkew = 5.0;
static const NSUInteger kNonceHexLen  = 32;   /* 16 bytes, hex-encoded */
static const off_t    kMaxGrantBytes  = 4096;
static NSString *const kGrantPurpose  = @"screensaver-unlock";
/* X9.63 uncompressed P-256 point: 0x04 || X || Y. */
static const NSUInteger kPublicKeyBytes = 65;
/* DER ECDSA P-256 signatures are variable-length; these are sanity bounds, not
 * the check. SecKeyVerifySignature is the check. */
static const NSUInteger kMinSignatureBytes = 8;
static const NSUInteger kMaxSignatureBytes = 150;
static const off_t    kMaxDeviceIDBytes = 256;
static const NSUInteger kMaxUsernameBytes = 256;

#pragma mark - Plugin scaffolding

typedef struct PluginRecord {
    const AuthorizationCallbacks *callbacks;
} PluginRecord;

typedef struct MechanismRecord {
    AuthorizationEngineRef engine;
    PluginRecord          *plugin;
    BOOL                   completionEligible;
    char                   completionNonce[33];
} MechanismRecord;

/*
 * Reads the public Authorization environment username for this exact engine
 * evaluation. AuthorizationPlugin.h says GetContextValue returns a borrowed
 * AuthorizationValue, and AuthorizationTags.h defines this key's bytes as the
 * username itself; copy by explicit length and never assume NUL termination.
 * Missing, malformed, or unavailable context is a denial.
 */
static NSString *AgentHaloAuthorizationUsername(MechanismRecord *mechanism) {
    if (mechanism == NULL || mechanism->plugin == NULL ||
        mechanism->plugin->callbacks == NULL ||
        mechanism->plugin->callbacks->GetContextValue == NULL) {
        return nil;
    }
    AuthorizationContextFlags flags = 0;
    const AuthorizationValue *value = NULL;
    OSStatus status = mechanism->plugin->callbacks->GetContextValue(
        mechanism->engine, kAuthorizationEnvironmentUsername, &flags, &value);
    (void)flags;
    if (status != errAuthorizationSuccess || value == NULL || value->data == NULL ||
        value->length == 0 || value->length > kMaxUsernameBytes ||
        memchr(value->data, '\0', value->length) != NULL) {
        return nil;
    }
    NSString *username = [[NSString alloc] initWithBytes:value->data
                                                  length:value->length
                                                encoding:NSUTF8StringEncoding];
    return username.length > 0 ? username : nil;
}

/* Both signed claims must match the transaction username, and resolving that
 * public account name must produce the signed uid. Comparing only names or only
 * uids would leave an avoidable ambiguity at the Fast User Switching boundary. */
static BOOL AgentHaloClaimsMatchAuthorizationUser(NSDictionary *claims,
                                           NSString *authorizationUsername) {
    if (authorizationUsername.length == 0) return NO;
    NSString *claimedUsername = claims[@"console_username"];
    NSNumber *claimedUID = claims[@"console_uid"];
    if (![claimedUsername isKindOfClass:NSString.class] ||
        ![claimedUID isKindOfClass:NSNumber.class] ||
        ![claimedUsername isEqualToString:authorizationUsername] ||
        CFGetTypeID((__bridge CFTypeRef)claimedUID) == CFBooleanGetTypeID()) {
        return NO;
    }
    unsigned long long numericUID = claimedUID.unsignedLongLongValue;
    if (numericUID == 0 || numericUID > UINT32_MAX) return NO;

    const char *name = authorizationUsername.UTF8String;
    if (name == NULL || strlen(name) == 0 || strlen(name) > kMaxUsernameBytes) return NO;
    struct passwd record;
    struct passwd *resolved = NULL;
    char buffer[16 * 1024];
    if (getpwnam_r(name, &record, buffer, sizeof(buffer), &resolved) != 0 ||
        resolved == NULL) {
        return NO;
    }
    return (unsigned long long)record.pw_uid == numericUID;
}

#pragma mark - Grant verification

/*
 * Reads the grant file exactly once into memory.
 *
 * The open is O_NOFOLLOW so a symlink planted at the path cannot redirect a
 * root-context read, and the descriptor is fstat'd (not stat'd by path) so the
 * bytes verified are the bytes from the file whose ownership was checked. The
 * file must be a regular file owned by root and no larger than a grant can be.
 */
static void AgentHaloReleaseGrantLock(int fd) {
    if (fd < 0) return;
    flock(fd, LOCK_UN);
    close(fd);
}

static NSData *AgentHaloLoadGrantBytes(int *lockedGrantFD) {
    if (lockedGrantFD != NULL) *lockedGrantFD = -1;
    int fd = open(AGENTHALO_GRANT_PATH, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0) {
        return nil;
    }
    if (flock(fd, LOCK_SH) != 0) {
        close(fd);
        return nil;
    }
    struct stat st;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_uid != 0 ||
        st.st_size <= 0 || st.st_size > kMaxGrantBytes) {
        AgentHaloReleaseGrantLock(fd);
        return nil;
    }
    NSMutableData *buffer = [NSMutableData dataWithLength:(NSUInteger)st.st_size];
    ssize_t got = read(fd, buffer.mutableBytes, (size_t)st.st_size);
    if (got != st.st_size) {
        AgentHaloReleaseGrantLock(fd);
        return nil;
    }
    if (lockedGrantFD != NULL) {
        *lockedGrantFD = fd;
    } else {
        AgentHaloReleaseGrantLock(fd);
    }
    return buffer;
}

/* Loads the provisioned P-256 public key (base64, root-owned, 0600). */
static NSData *AgentHaloLoadPublicKey(void) {
    int fd = open(AGENTHALO_PUBKEY_PATH, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0) {
        return nil;
    }
    struct stat st;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_uid != 0 ||
        st.st_size <= 0 || st.st_size > 256) {
        close(fd);
        return nil;
    }
    NSMutableData *raw = [NSMutableData dataWithLength:(NSUInteger)st.st_size];
    ssize_t got = read(fd, raw.mutableBytes, (size_t)st.st_size);
    close(fd);
    if (got != st.st_size) {
        return nil;
    }
    NSString *text = [[[NSString alloc] initWithData:raw encoding:NSUTF8StringEncoding]
                      stringByTrimmingCharactersInSet:[NSCharacterSet whitespaceAndNewlineCharacterSet]];
    if (text.length == 0) {
        return nil;
    }
    return [[NSData alloc] initWithBase64EncodedString:text options:0];
}

/*
 * Verifies the grant signature over payload using the provisioned key:
 * ECDSA P-256 over SHA-256, mirrored by VerifyPayloadSignature in Go.
 *
 * The algorithm is chosen by what this bundle is allowed to depend on, not by
 * preference. Ed25519 is the better primitive, but SecKey's Ed25519 constants
 * are SPI — exported by Security.tbd and declared in no public header. A
 * mechanism that binds a private symbol stops loading the day Apple drops it,
 * and this mechanism sits in the screensaver-unlock right, so a bundle that
 * cannot load is exactly the lockout direction commitment 1 forbids.
 * kSecKeyAlgorithmECDSASignatureMessageX962SHA256 is public API and hashes the
 * message itself, so the signed bytes here are the payload bytes, unhashed by
 * the caller.
 *
 * Every failure — a key that will not build, an unavailable algorithm, a bad
 * signature — returns NO, and NO means the plugin stays transparent and the
 * password challenge proceeds unchanged.
 */
static BOOL AgentHaloVerifySignature(NSData *payload, NSData *signature, NSData *publicKey) {
    if (payload.length == 0 || publicKey.length != kPublicKeyBytes ||
        signature.length < kMinSignatureBytes || signature.length > kMaxSignatureBytes) {
        return NO;
    }
    /* SecKeyCreateWithData reads an EC public key as an X9.63 point. Requiring
     * the uncompressed marker keeps this to the one encoding the agent
     * publishes rather than whatever else might parse. */
    if (((const uint8_t *)publicKey.bytes)[0] != 0x04) {
        return NO;
    }
    NSDictionary *attrs = @{
        (__bridge id)kSecAttrKeyType:       (__bridge id)kSecAttrKeyTypeECSECPrimeRandom,
        (__bridge id)kSecAttrKeyClass:      (__bridge id)kSecAttrKeyClassPublic,
        (__bridge id)kSecAttrKeySizeInBits: @256,
    };
    CFErrorRef error = NULL;
    SecKeyRef key = SecKeyCreateWithData((__bridge CFDataRef)publicKey,
                                         (__bridge CFDictionaryRef)attrs, &error);
    if (key == NULL) {
        if (error) CFRelease(error);
        return NO;
    }
    Boolean ok = SecKeyVerifySignature(key, kSecKeyAlgorithmECDSASignatureMessageX962SHA256,
                                       (__bridge CFDataRef)payload,
                                       (__bridge CFDataRef)signature, &error);
    CFRelease(key);
    if (error) CFRelease(error);
    return ok ? YES : NO;
}

/*
 * Consumes a nonce atomically. Returns YES only if this call is the one that
 * created the ledger entry.
 *
 * O_EXCL is what makes single-use real: two concurrent verifiers cannot both
 * win. EEXIST means the grant was already used and must be refused. Any other
 * failure also returns NO — an unrecordable consumption is not a permitted
 * unlock.
 */
static BOOL AgentHaloConsumeNonce(NSString *nonceHex, long long expiresAt) {
    if (nonceHex.length != kNonceHexLen) {
        return NO;
    }
    /* The nonce becomes a filename, so accept only lowercase hex. This is a
     * containment check, not a formatting nicety. */
    for (NSUInteger i = 0; i < nonceHex.length; i++) {
        unichar c = [nonceHex characterAtIndex:i];
        BOOL hex = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
        if (!hex) return NO;
    }
    mkdir(AGENTHALO_LEDGER_DIR, 0700);
    NSString *path = [NSString stringWithFormat:@"%s/%@", AGENTHALO_LEDGER_DIR, nonceHex];
    int fd = open(path.fileSystemRepresentation,
                  O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0600);
    if (fd < 0) {
        return NO;  /* EEXIST (replay) or any other failure: refuse. */
    }
    char line[64];
    int n = snprintf(line, sizeof(line), "%lld\n", expiresAt);
    if (n > 0) {
        ssize_t ignored = write(fd, line, (size_t)n);
        (void)ignored;
    }
    close(fd);
    return YES;
}

/*
 * Publishes proof that this exact nonce reached the privileged verifier.
 *
 * Watching the session become unlocked is not enough to attribute the unlock
 * to this grant: a person, Apple Watch, or another authorization path could
 * have won at the same instant. The user-session controller therefore requires
 * both the unlocked state and this root-owned exact-nonce receipt.
 *
 * A temporary root-owned file is fsync'd and atomically renamed into place, so
 * the reader sees either the previous complete nonce or this complete nonce,
 * never a partial write. Failure is a refusal. The nonce has already been
 * burned at that point, which loses one attempt but cannot create authority.
 */
static BOOL AgentHaloWriteAll(int fd, const void *bytes, size_t length) {
    const uint8_t *cursor = bytes;
    size_t remaining = length;
    while (remaining > 0) {
        ssize_t written = write(fd, cursor, remaining);
        if (written < 0 && errno == EINTR) continue;
        if (written <= 0) return NO;
        cursor += written;
        remaining -= (size_t)written;
    }
    return YES;
}

static BOOL AgentHaloPublishNonceProof(NSString *nonceHex,
                                const char *destinationPath,
                                NSString *temporaryStem) {
    /* The controller treats this as privileged proof. A mechanism loaded in an
     * unexpected non-root context must never manufacture a user-owned lookalike
     * and then authorize. */
    if (geteuid() != 0) return NO;
    NSData *nonce = [nonceHex dataUsingEncoding:NSASCIIStringEncoding];
    if (nonce.length != kNonceHexLen) return NO;

    NSString *temporary = [NSString stringWithFormat:
        @AGENTHALO_LOCKED_USE_DIR "/.%@.%d.%08x",
        temporaryStem, getpid(), arc4random()];
    int fd = open(temporary.fileSystemRepresentation,
                  O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0644);
    if (fd < 0) return NO;

    BOOL ok = YES;
    struct stat st;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_uid != 0) ok = NO;
    if (ok && fchmod(fd, 0644) != 0) ok = NO;
    if (ok && !AgentHaloWriteAll(fd, nonce.bytes, nonce.length)) ok = NO;
    if (ok && fsync(fd) != 0) ok = NO;
    if (close(fd) != 0) ok = NO;

    if (ok && rename(temporary.fileSystemRepresentation, destinationPath) != 0) {
        ok = NO;
    }
    if (!ok) {
        unlink(temporary.fileSystemRepresentation);
        return NO;
    }

    /* Persist the rename as well as the file contents. A receipt lost across a
     * crash is safe, but treating a successful publish as durable makes the
     * controller's proof match what this function returned. */
    int directory = open(AGENTHALO_LOCKED_USE_DIR, O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (directory < 0) return NO;
    BOOL synced = fsync(directory) == 0;
    close(directory);
    return synced;
}

/*
 * Drops ledger entries whose grants can no longer be valid.
 *
 * Nothing else prunes this directory — the agent's copy of PruneNonces runs
 * against its own path, not this root-owned one — so without this the ledger
 * grows one small file per unlock forever. Entries are removed strictly by
 * recorded expiry plus the full skew allowance, never by count or age of the
 * file, so pruning can never forget a nonce that is still replayable.
 */
static void AgentHaloPruneLedger(void) {
    NSFileManager *fm = NSFileManager.defaultManager;
    NSString *dir = @AGENTHALO_LEDGER_DIR;
    NSArray<NSString *> *names = [fm contentsOfDirectoryAtPath:dir error:NULL];
    if (names.count == 0) {
        return;
    }
    NSTimeInterval now = [[NSDate date] timeIntervalSince1970];
    for (NSString *name in names) {
        NSString *path = [dir stringByAppendingPathComponent:name];
        NSString *body = [NSString stringWithContentsOfFile:path
                                                   encoding:NSUTF8StringEncoding error:NULL];
        if (body.length == 0) {
            continue;
        }
        long long expires = [body longLongValue];
        if (expires <= 0) {
            continue;
        }
        if (now > (NSTimeInterval)expires + kMaxGrantTTL + kMaxClockSkew) {
            [fm removeItemAtPath:path error:NULL];
        }
    }
}

/*
 * Reads the device id this Mac was bound to at install time. install.sh refuses
 * to install without this binding, and a missing or unreadable file must remain
 * a denial here: silently skipping the comparison would turn corruption of a
 * root-owned security input into broader authorization.
 *
 * The read is O_NOFOLLOW and fstat-checked for root ownership for the same
 * reason as the grant and the key: a file a non-root user could plant must not
 * decide which grants this Mac accepts.
 */
static NSString *AgentHaloExpectedDeviceID(void) {
    int fd = open(AGENTHALO_DEVICE_PATH, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0) {
        return nil;
    }
    struct stat st;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_uid != 0 ||
        st.st_size <= 0 || st.st_size > kMaxDeviceIDBytes) {
        close(fd);
        return nil;
    }
    NSMutableData *raw = [NSMutableData dataWithLength:(NSUInteger)st.st_size];
    ssize_t got = read(fd, raw.mutableBytes, (size_t)st.st_size);
    close(fd);
    if (got != st.st_size) {
        return nil;
    }
    NSString *text = [[[NSString alloc] initWithData:raw encoding:NSUTF8StringEncoding]
                      stringByTrimmingCharactersInSet:[NSCharacterSet whitespaceAndNewlineCharacterSet]];
    return text.length > 0 ? text : nil;
}

/*
 * The full grant check. Returns YES only when every condition holds and the
 * nonce has been successfully consumed.
 *
 * Ordering matters: signature first (so nothing downstream parses attacker-
 * chosen structure that was not signed), then the semantic checks, then
 * consumption last so a grant that fails any check is not burned.
 */
static BOOL AgentHaloGrantAllowsUnlock(NSString *authorizationUsername,
                                NSString **consumedNonce,
                                int *lockedGrantFD) {
    __block int grantFD = -1;
    if (lockedGrantFD != NULL) *lockedGrantFD = -1;
    @try {
      @autoreleasepool {
        if (consumedNonce != NULL) *consumedNonce = nil;
        NSData *envelopeData = AgentHaloLoadGrantBytes(&grantFD);
        if (envelopeData == nil) return NO;

        NSData *publicKey = AgentHaloLoadPublicKey();
        if (publicKey == nil) return NO;

        NSDictionary *envelope = [NSJSONSerialization JSONObjectWithData:envelopeData
                                                                 options:0 error:NULL];
        if (![envelope isKindOfClass:NSDictionary.class]) return NO;

        NSString *payloadB64 = envelope[@"payload"];
        NSString *signatureB64 = envelope[@"signature"];
        if (![payloadB64 isKindOfClass:NSString.class] ||
            ![signatureB64 isKindOfClass:NSString.class]) {
            return NO;
        }
        NSData *payload = [[NSData alloc] initWithBase64EncodedString:payloadB64 options:0];
        NSData *signature = [[NSData alloc] initWithBase64EncodedString:signatureB64 options:0];
        if (payload == nil || signature == nil) return NO;

        if (!AgentHaloVerifySignature(payload, signature, publicKey)) return NO;

        /* Parse only the bytes the signature covered. */
        NSDictionary *claims = [NSJSONSerialization JSONObjectWithData:payload options:0 error:NULL];
        if (![claims isKindOfClass:NSDictionary.class]) return NO;

        NSNumber *version = claims[@"v"];
        if (![version isKindOfClass:NSNumber.class] || version.intValue != kGrantVersion) return NO;

        NSString *purpose = claims[@"purpose"];
        if (![purpose isKindOfClass:NSString.class] || ![purpose isEqualToString:kGrantPurpose]) {
            return NO;
        }

        /* Bind the grant to this machine. The Swift mirror performs the same
         * check; without it a grant minted for one Mac would verify on any
         * other Mac provisioned with the same public key. */
        NSString *device = claims[@"device_id"];
        if (![device isKindOfClass:NSString.class] || device.length == 0) return NO;
        NSString *expectedDevice = AgentHaloExpectedDeviceID();
        if (expectedDevice.length == 0 || ![device isEqualToString:expectedDevice]) return NO;

        if (!AgentHaloClaimsMatchAuthorizationUser(claims, authorizationUsername)) return NO;

        NSString *nonce = claims[@"nonce"];
        if (![nonce isKindOfClass:NSString.class] || nonce.length != kNonceHexLen) return NO;

        NSNumber *issuedAt = claims[@"issued_at"];
        NSNumber *expiresAt = claims[@"expires_at"];
        if (![issuedAt isKindOfClass:NSNumber.class] || ![expiresAt isKindOfClass:NSNumber.class]) {
            return NO;
        }

        NSTimeInterval issued = issuedAt.doubleValue;
        NSTimeInterval expires = expiresAt.doubleValue;
        NSTimeInterval now = [[NSDate date] timeIntervalSince1970];

        /* The grant does not get to declare its own longevity. A lifetime past
         * the ceiling is rejected, not shortened. */
        if (expires <= issued) return NO;
        if (expires - issued > kMaxGrantTTL) return NO;
        if (issued > now + kMaxClockSkew) return NO;
        if (now > expires) return NO;

        /* Consume last: only a grant that passed every check is burned.
         * MechanismInvoke keeps the shared grant lock returned here while it
         * publishes pending, submits Allow, and publishes the final receipt.
         * Controller withdrawal takes the exclusive side of the same lock. */
        if (!AgentHaloConsumeNonce(nonce, (long long)expires)) {
            return NO;
        }
        if (consumedNonce != NULL) *consumedNonce = [nonce copy];
        /* Housekeeping only, and only after a decision is already made, so it
         * can never influence whether this unlock was allowed. */
        AgentHaloPruneLedger();
        if (lockedGrantFD != NULL) {
            *lockedGrantFD = grantFD;
            grantFD = -1;
        }
        return YES;
      }
    } @finally {
        AgentHaloReleaseGrantLock(grantFD);
    }
}

#pragma mark - Mechanism entry points

static OSStatus MechanismCreate(AuthorizationPluginRef inPlugin,
                                AuthorizationEngineRef inEngine,
                                AuthorizationMechanismId mechanismId,
                                AuthorizationMechanismRef *outMechanism) {
    (void)mechanismId;
    MechanismRecord *mechanism = (MechanismRecord *)calloc(1, sizeof(MechanismRecord));
    if (mechanism == NULL) return errAuthorizationInternal;
    mechanism->engine = inEngine;
    mechanism->plugin = (PluginRecord *)inPlugin;
    *outMechanism = (AuthorizationMechanismRef)mechanism;
    return errAuthorizationSuccess;
}

/*
 * Invoke is called during the unlock flow.
 *
 * This mechanism is one branch of the screensaver right's k-of-n rule. Allow
 * therefore authorizes the unlock; Deny declines this branch and leaves the
 * ordinary login-window branch available. It is not an all-mechanisms chain
 * where Allow would merely mean "continue".
 */
static OSStatus MechanismInvoke(AuthorizationMechanismRef inMechanism) {
    MechanismRecord *mechanism = (MechanismRecord *)inMechanism;
    /* RequestInterrupt permits the engine to invoke one mechanism instance
     * again. A later deny/cancel must not inherit an earlier successful nonce
     * and publish a false terminal from Destroy. */
    mechanism->completionEligible = NO;
    memset(mechanism->completionNonce, 0, sizeof(mechanism->completionNonce));

    /*
     * Allow only on a grant that verified and was consumed. Everything else
     * denies, which hands the decision to the next branch of the right — the
     * ordinary password prompt.
     *
     * An exception is a refusal, not a shrug: if the check could not complete,
     * this mechanism has no basis to authorize anything.
     */
    BOOL allowed = NO;
    NSString *consumedNonce = nil;
    int lockedGrantFD = -1;
    @try {
        NSString *authorizationUsername = AgentHaloAuthorizationUsername(mechanism);
        if (authorizationUsername != nil) {
            allowed = AgentHaloGrantAllowsUnlock(
                authorizationUsername, &consumedNonce, &lockedGrantFD);
        }
    } @catch (__unused NSException *exception) {
        allowed = NO;
    }

    if (!allowed || consumedNonce == nil) {
        AgentHaloReleaseGrantLock(lockedGrantFD);
        return mechanism->plugin->callbacks->SetResult(
            mechanism->engine, kAuthorizationResultDeny);
    }

    /* Two durable root-owned phases close both crash windows. `pending` is
     * written before Allow and means this exact authorization may still land;
     * the final `receipt` is written only after SetResult(Allow) succeeds and
     * remains the proof required to open the window. A pre-Allow proof alone
     * cannot be mistaken for success won by this branch, while a crash after
     * Allow can no longer leave the controller unaware of a late transition. */
    if (!AgentHaloPublishNonceProof(
            consumedNonce, AGENTHALO_PENDING_RECEIPT_PATH, @"receipt.pending")) {
        AgentHaloReleaseGrantLock(lockedGrantFD);
        return errAuthorizationInternal;
    }

    OSStatus status = mechanism->plugin->callbacks->SetResult(
        mechanism->engine, kAuthorizationResultAllow);
    if (status != errAuthorizationSuccess) {
        AgentHaloReleaseGrantLock(lockedGrantFD);
        return status;
    }

    /* Receipt failure cannot be repaired by silently accepting weaker proof.
     * The allow decision may already be in flight, so the session controller
     * will keep its shield up, refuse to open the window, and relock when the
     * receipt is absent. Returning an error also tells the authorization engine
     * this mechanism did not complete cleanly. */
    if (!AgentHaloPublishNonceProof(consumedNonce, AGENTHALO_RECEIPT_PATH, @"receipt")) {
        AgentHaloReleaseGrantLock(lockedGrantFD);
        return errAuthorizationInternal;
    }
    NSData *completionNonce = [consumedNonce dataUsingEncoding:NSASCIIStringEncoding];
    if (completionNonce.length != kNonceHexLen) {
        AgentHaloReleaseGrantLock(lockedGrantFD);
        return errAuthorizationInternal;
    }
    memcpy(mechanism->completionNonce, completionNonce.bytes, kNonceHexLen);
    mechanism->completionNonce[kNonceHexLen] = '\0';
    mechanism->completionEligible = YES;
    AgentHaloReleaseGrantLock(lockedGrantFD);
    return status;
}

static OSStatus MechanismDeactivate(AuthorizationMechanismRef inMechanism) {
    MechanismRecord *mechanism = (MechanismRecord *)inMechanism;
    /* Deactivation may be cancellation and is not a successful transaction
     * terminal. Only Destroy for an Invoke that published the final receipt
     * may publish receipt.complete. */
    mechanism->completionEligible = NO;
    memset(mechanism->completionNonce, 0, sizeof(mechanism->completionNonce));
    return mechanism->plugin->callbacks->DidDeactivate(mechanism->engine);
}

static OSStatus MechanismDestroy(AuthorizationMechanismRef inMechanism) {
    MechanismRecord *mechanism = (MechanismRecord *)inMechanism;
    BOOL completed = YES;
    if (mechanism->completionEligible) {
        NSString *nonce = [NSString stringWithUTF8String:mechanism->completionNonce];
        completed = nonce != nil && AgentHaloPublishNonceProof(
            nonce, AGENTHALO_COMPLETION_RECEIPT_PATH, @"receipt.complete");
    }
    free(mechanism);
    return completed ? errAuthorizationSuccess : errAuthorizationInternal;
}

static OSStatus PluginDestroy(AuthorizationPluginRef inPlugin) {
    free(inPlugin);
    return errAuthorizationSuccess;
}

static AuthorizationPluginInterface gPluginInterface = {
    kAuthorizationPluginInterfaceVersion,
    PluginDestroy,
    MechanismCreate,
    MechanismInvoke,
    MechanismDeactivate,
    MechanismDestroy,
};

/* Entry point the Security framework looks up by name. */
OSStatus AuthorizationPluginCreate(const AuthorizationCallbacks *callbacks,
                                   AuthorizationPluginRef *outPlugin,
                                   const AuthorizationPluginInterface **outPluginInterface) {
    PluginRecord *plugin = (PluginRecord *)calloc(1, sizeof(PluginRecord));
    if (plugin == NULL) return errAuthorizationInternal;
    plugin->callbacks = callbacks;
    *outPlugin = (AuthorizationPluginRef)plugin;
    *outPluginInterface = &gPluginInterface;
    return errAuthorizationSuccess;
}
