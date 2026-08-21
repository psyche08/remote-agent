/*
 * interop_check.m — runs the plug-in's real verifier against bytes the Swift
 * signer produced.
 *
 * The grant contract is implemented twice, in two languages, and only this
 * side enforces anything. CI can compile neither this bundle nor test it
 * against the Swift minter, so "both sides build" has never meant "both sides
 * agree". They can disagree silently: the agent mints grants the plug-in
 * refuses forever, and the only symptom is a Mac that never unlocks.
 *
 * The plug-in's verifier is static, so this includes the translation unit
 * rather than linking it — the code under test is the shipped code, not a
 * copy. Nothing here touches the grant, key, or ledger files;
 * AgentHaloVerifySignature
 * is pure.
 *
 * Driven by mac/preflight.sh. Usage:
 *   interop_check <pubkey-b64> <payload-b64> <signature-b64>
 *   interop_check --claims-user <payload-b64> <authorization-username>
 * Exit: 0 signature verified · 1 rejected · 2 bad usage or malformed base64
 */

#include "AgentHaloLockedUse.m"

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        if (argc != 4) {
            fprintf(stderr, "usage: %s <pubkey-b64> <payload-b64> <signature-b64>\n", argv[0]);
            return 2;
        }
        NSData *(^decode)(const char *) = ^NSData *(const char *arg) {
            NSString *text = [NSString stringWithUTF8String:arg];
            return text ? [[NSData alloc] initWithBase64EncodedString:text options:0] : nil;
        };
        if (strcmp(argv[1], "--claims-user") == 0) {
            NSData *payload = decode(argv[2]);
            NSString *username = [NSString stringWithUTF8String:argv[3]];
            NSDictionary *claims = payload == nil ? nil :
                [NSJSONSerialization JSONObjectWithData:payload options:0 error:NULL];
            if (![claims isKindOfClass:NSDictionary.class] || username == nil) return 2;
            return AgentHaloClaimsMatchAuthorizationUser(claims, username) ? 0 : 1;
        }
        NSData *pub = decode(argv[1]);
        NSData *payload = decode(argv[2]);
        NSData *signature = decode(argv[3]);
        if (pub == nil || payload == nil || signature == nil) {
            fprintf(stderr, "one or more arguments are not valid base64\n");
            return 2;
        }
        return AgentHaloVerifySignature(payload, signature, pub) ? 0 : 1;
    }
}
