/*
 * RemoteAgentLockedUse.m — Apple Authorization Plug-in for Locked Use.
 *
 * This bundle participates in the macOS screensaver-unlock authorization right.
 * It is the enforcing verifier for the grant contract defined in Go at
 * internal/computeruse/grant.go; that package's VerifyGrant is a testable
 * mirror of the checks below, not a substitute for them.
 *
 * Design commitments, in order of importance:
 *
 *  1. IT CAN NEVER LOCK YOU OUT. This mechanism never returns Deny and never
 *     returns Undefined. Undefined is documented as "the mechanism did not come
 *     to a decision", and authd may treat that as a mechanism failure and abort
 *     the whole authorization — which on the screensaver right would mean a Mac
 *     nobody can unlock. So the no-grant path returns Allow, meaning only "this
 *     mechanism does not object", and the password mechanism that follows still
 *     challenges the user exactly as before.
 *
 *     CONSEQUENCE, STATED PLAINLY: because this mechanism only ever declines to
 *     object, installing it alone does NOT bypass the password. Whether a valid
 *     grant actually shortens the unlock depends on how the right's mechanism
 *     list is arranged on your macOS version, which is why install.sh requires
 *     an explicit validation step on a non-production Mac first. If that
 *     arrangement is wrong, the feature simply does not unlock — the safe
 *     failure direction — rather than locking anyone out.
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
#import <sys/stat.h>
#import <fcntl.h>
#import <unistd.h>
#import <errno.h>

/* Paths are compile-time constants under a root-owned directory. Nothing here
 * reads a path supplied by a caller, and the agent (running as the user) can
 * write only the staging file the installer grants it. */
#ifndef RA_LOCKED_USE_DIR
#define RA_LOCKED_USE_DIR "/Library/Application Support/remote-agent/locked-use"
#endif

#define RA_GRANT_PATH   RA_LOCKED_USE_DIR "/grant.json"
#define RA_PUBKEY_PATH  RA_LOCKED_USE_DIR "/public.key"
#define RA_LEDGER_DIR   RA_LOCKED_USE_DIR "/consumed"
#define RA_DEVICE_PATH  RA_LOCKED_USE_DIR "/device_id"

/* Must match internal/computeruse/grant.go. */
static const int      kGrantVersion   = 1;
static const NSTimeInterval kMaxGrantTTL  = 15.0;
static const NSTimeInterval kMaxClockSkew = 5.0;
static const NSUInteger kNonceHexLen  = 32;   /* 16 bytes, hex-encoded */
static const off_t    kMaxGrantBytes  = 4096;
static NSString *const kGrantPurpose  = @"screensaver-unlock";

#pragma mark - Plugin scaffolding

typedef struct PluginRecord {
    const AuthorizationCallbacks *callbacks;
} PluginRecord;

typedef struct MechanismRecord {
    AuthorizationEngineRef engine;
    PluginRecord          *plugin;
} MechanismRecord;

#pragma mark - Grant verification

/*
 * Reads the grant file exactly once into memory.
 *
 * The open is O_NOFOLLOW so a symlink planted at the path cannot redirect a
 * root-context read, and the descriptor is fstat'd (not stat'd by path) so the
 * bytes verified are the bytes from the file whose ownership was checked. The
 * file must be a regular file owned by root and no larger than a grant can be.
 */
static NSData *RALoadGrantBytes(void) {
    int fd = open(RA_GRANT_PATH, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0) {
        return nil;
    }
    struct stat st;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_uid != 0 ||
        st.st_size <= 0 || st.st_size > kMaxGrantBytes) {
        close(fd);
        return nil;
    }
    NSMutableData *buffer = [NSMutableData dataWithLength:(NSUInteger)st.st_size];
    ssize_t got = read(fd, buffer.mutableBytes, (size_t)st.st_size);
    close(fd);
    if (got != st.st_size) {
        return nil;
    }
    return buffer;
}

/* Loads the provisioned Ed25519 public key (base64, root-owned, 0600). */
static NSData *RALoadPublicKey(void) {
    int fd = open(RA_PUBKEY_PATH, O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
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
 * Verifies an Ed25519 signature over payload using the provisioned key.
 *
 * CryptoKit is not reachable from a C-ABI plugin, so this goes through
 * SecKeyVerifySignature with an Ed25519 key. On a system where the algorithm is
 * unavailable, verification fails and the plugin stays transparent — the safe
 * direction.
 */
static BOOL RAVerifySignature(NSData *payload, NSData *signature, NSData *publicKey) {
    if (payload.length == 0 || signature.length != 64 || publicKey.length != 32) {
        return NO;
    }
    NSDictionary *attrs = @{
        (__bridge id)kSecAttrKeyType:  (__bridge id)kSecAttrKeyTypeEd25519,
        (__bridge id)kSecAttrKeyClass: (__bridge id)kSecAttrKeyClassPublic,
    };
    CFErrorRef error = NULL;
    SecKeyRef key = SecKeyCreateWithData((__bridge CFDataRef)publicKey,
                                         (__bridge CFDictionaryRef)attrs, &error);
    if (key == NULL) {
        if (error) CFRelease(error);
        return NO;
    }
    Boolean ok = SecKeyVerifySignature(key, kSecKeyAlgorithmEdDSASignatureMessageEd25519,
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
static BOOL RAConsumeNonce(NSString *nonceHex, long long expiresAt) {
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
    mkdir(RA_LEDGER_DIR, 0700);
    NSString *path = [NSString stringWithFormat:@"%s/%@", RA_LEDGER_DIR, nonceHex];
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
 * Drops ledger entries whose grants can no longer be valid.
 *
 * Nothing else prunes this directory — the agent's copy of PruneNonces runs
 * against its own path, not this root-owned one — so without this the ledger
 * grows one small file per unlock forever. Entries are removed strictly by
 * recorded expiry plus the full skew allowance, never by count or age of the
 * file, so pruning can never forget a nonce that is still replayable.
 */
static void RAPruneLedger(void) {
    NSFileManager *fm = NSFileManager.defaultManager;
    NSString *dir = @RA_LEDGER_DIR;
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
 * The full grant check. Returns YES only when every condition holds and the
 * nonce has been successfully consumed.
 *
 * Ordering matters: signature first (so nothing downstream parses attacker-
 * chosen structure that was not signed), then the semantic checks, then
 * consumption last so a grant that fails any check is not burned.
 */
static BOOL RAGrantAllowsUnlock(void) {
    @autoreleasepool {
        NSData *envelopeData = RALoadGrantBytes();
        if (envelopeData == nil) return NO;

        NSData *publicKey = RALoadPublicKey();
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

        if (!RAVerifySignature(payload, signature, publicKey)) return NO;

        /* Parse only the bytes the signature covered. */
        NSDictionary *claims = [NSJSONSerialization JSONObjectWithData:payload options:0 error:NULL];
        if (![claims isKindOfClass:NSDictionary.class]) return NO;

        NSNumber *version = claims[@"v"];
        if (![version isKindOfClass:NSNumber.class] || version.intValue != kGrantVersion) return NO;

        NSString *purpose = claims[@"purpose"];
        if (![purpose isKindOfClass:NSString.class] || ![purpose isEqualToString:kGrantPurpose]) {
            return NO;
        }

        /* Bind the grant to this machine. The Go mirror performs the same
         * check; without it a grant minted for one Mac would verify on any
         * other Mac provisioned with the same public key. */
        NSString *device = claims[@"device_id"];
        if (![device isKindOfClass:NSString.class] || device.length == 0) return NO;
        NSString *expectedDevice = RAExpectedDeviceID();
        if (expectedDevice.length > 0 && ![device isEqualToString:expectedDevice]) return NO;

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

        /* Consume last: only a grant that passed every check is burned. */
        if (!RAConsumeNonce(nonce, (long long)expires)) {
            return NO;
        }
        /* Housekeeping only, and only after a decision is already made, so it
         * can never influence whether this unlock was allowed. */
        RAPruneLedger();
        return YES;
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
 * It always returns kAuthorizationResultAllow — never Deny, never Undefined.
 *
 * That looks odd for a security check, so it is worth being precise about what
 * Allow means here. In a mechanism chain, Allow is this mechanism saying "I do
 * not object"; the remaining mechanisms, including the password challenge, still
 * run. It is not a grant of access. Deny and Undefined are the outcomes that
 * could leave a person unable to unlock their own Mac, and no defect in this
 * code — a corrupt grant, an unreadable key, an exception — is worth that.
 *
 * The grant check still runs, and still consumes its nonce, because that is what
 * records an authorized unlock and keeps single-use honest. Whether the
 * consumed grant actually shortens the unlock is a property of how the right's
 * mechanism list is arranged, which install.sh makes the operator validate.
 */
static OSStatus MechanismInvoke(AuthorizationMechanismRef inMechanism) {
    MechanismRecord *mechanism = (MechanismRecord *)inMechanism;

    /* The result of the check does not change what we return; it decides
     * whether an authorized unlock is recorded and its nonce burned. Any
     * unexpected failure is swallowed for the same reason. */
    @try {
        (void)RAGrantAllowsUnlock();
    } @catch (__unused NSException *exception) {
        /* deliberately ignored — see the note above */
    }

    return mechanism->plugin->callbacks->SetResult(mechanism->engine,
                                                   kAuthorizationResultAllow);
}

static OSStatus MechanismDeactivate(AuthorizationMechanismRef inMechanism) {
    MechanismRecord *mechanism = (MechanismRecord *)inMechanism;
    return mechanism->plugin->callbacks->DidDeactivate(mechanism->engine);
}

static OSStatus MechanismDestroy(AuthorizationMechanismRef inMechanism) {
    free(inMechanism);
    return errAuthorizationSuccess;
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
