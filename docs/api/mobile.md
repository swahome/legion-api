# Mobile app API (legacy compatible)

The released mobile app keeps working against Legion. It calls the same
actions with the same encrypted payloads; only its base URL changes:

```
old: https://<chamasoft host>/mobile/<action>
new: https://legion.com/v1/legacy/mobile/<action>
```

So the app's base URL becomes `https://legion.com/v1/legacy/`. It sits under
`/v1`, so the web server's existing `/v1` proxy covers it.

The full list of legacy actions, and which milestone ports each one, is in
`docs/mobile/legacy-mobile-api.md`.

## Setup

The API needs the two legacy key files. Copy them out of the old
`assets/certificates/` folder to the server, outside the repository, and set:

```sh
LEGION_MOBILE_SERVER_KEY=/etc/legion/10.184.38.64.key   # opens the app's requests
LEGION_MOBILE_APP_CERT=/etc/legion/mobile.crt           # seals replies for the app
```

With neither set, `/v1/legacy` isn't served. Setting only one is an error.
`mobile.crt` expired in 2021; that doesn't matter, because only its public key is
used, as before.

## What carries over

- **Encryption, both ways.** A request body is `base64(AES-128-CBC(json)):base64(iv)`, and its key travels RSA-wrapped in the `Secret` header. A reply is `{"secret", "body"}`, sealed for `mobile.crt`. This was checked against the real keys using the legacy PHP `Encryptdecrypt` code: it reads what Legion sends, and Legion reads what it sends.
- **Headers and answers.**
  - `Authorization: Bearer <token>`, `Secret` and `Versioncode` (or `VersionCode`) work as before.
  - Builds 73 and older are told to update.
  - Builds 74 to 77 get `{"response": {...}}`; newer builds get the object unwrapped.
  - Errors keep their legacy `status` codes and messages: `401` with no token, `400` for an expired token or a `user_id` that isn't the token's, `2` for an empty body, `3` for an unreadable one, and HTTP 404 for an unknown action.
- **Tokens.**
  - 32 characters, valid for 100 days, and each use keeps one alive for at least 30 more days.
  - The import carries over live app sessions from `user_pin_access_token`, so nobody is signed out by the switch.
  - Deactivating a user ends their app sessions at once.
- **Response shapes.** Ids and other database values are strings, names are upper-cased and phones have no `+`, as MySQL and the old code returned them. `group_roles` is an id-to-name object in name order, or `[]` when empty.

## Actions served so far

| Action | Notes |
|---|---|
| `mobile/login` | Phone or email and password. Uses the web login's throttle: 3 failures in 10 minutes lock that identity and IP |
| `mobile/generate_pin`, `mobile/resend_pin` | Send a 4-digit PIN. No SMS sender is configured until milestone 5, so for now these answer "Error occured sending pin" |
| `mobile/verify_pin` | Trades the PIN for a token. For a number with no account it returns `user_exists: 0` and the `unique_code` that sign-up needs |
| `mobile/get_user_checkin_data` | The user and their groups, with role, activation status and currency |

The other actions answer HTTP 404 until their milestone ports them.

## Deliberately not carried over

These were security holes in the old API:

- **Fixed back doors.** The token and secret `abcdefghhgfedcba`, and the IP allowlist (`127.0.0.1`, `197.237.106.185`), skipped authentication and encryption.
- **Fixed test PINs.** `1234` worked for a few numbers and emails.
- **PINs that never expired and could be guessed without limit.** PINs now last 10 minutes and lock after 5 wrong tries. They're stored as hashes, and each one works only once.
- **Plain-text logging.** Every payload, password and PIN went to `logs/*.dat`, and every OTP was emailed to an admin.
- **`mobile/get_all_otps`.** Any signed-in user could read everyone's PINs with it.
- **Plain-text tokens.** Access tokens were stored as they are; now only their SHA-256 is kept.
