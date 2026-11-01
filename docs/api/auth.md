# Authentication

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/v1/auth/login` | none | `{"identity": "<phone or email>", "password": "..."}` |
| POST | `/v1/auth/refresh` | refresh cookie | Rotates the refresh token and returns a new access token |
| POST | `/v1/auth/logout` | refresh cookie | Revokes the session (the whole refresh token family) |
| GET | `/v1/me` | Bearer | User, platform roles, group memberships (the group switcher) and the menus an admin switched off |
| GET | `/healthz` | none | Database ping |
| POST | `/v1/auth/signup` | none | `first_name`, `last_name`, `phone`, optional `email`, `password` (8 or more characters). 201 with a session, and a 6-digit code is texted to the phone. 409 `phone_taken` or `email_taken` |
| POST | `/v1/auth/verify-phone` | Bearer | `{"code": "123456"}`. Marks the phone verified. A group can only be created once it is |
| POST | `/v1/auth/verify-phone/resend` | Bearer | Texts a new code. 409 `already_verified` |
| POST | `/v1/auth/forgot-password` | none | `{"identity": "<phone or email>"}`. Always 202, so accounts can't be probed. A 6-digit code goes to the account's phone (email delivery isn't built) |
| POST | `/v1/auth/reset-password` | none | `identity`, `code`, `password`. Sets the password, marks the phone verified, signs out every other session and app, and returns a session |
| GET | `/v1/auth/invites/{token}` | none | The invite a member was texted: `first_name`, `group_name`, `invited_by`, `needs_password`. 410 `invite_expired` |
| POST | `/v1/auth/invites/{token}` | none | `{"password": "..."}`. Sets the invited member's password and returns a session. 409 `password_already_set` |

Login and refresh both return:

```json
{ "access_token": "...", "token_type": "Bearer", "expires_in": 900, "user": { ... } }
```

and set the `legion_refresh` cookie (HttpOnly, SameSite=Strict, Path=/v1/auth).

Errors always look like `{"error": {"code": "...", "message": "..."}}`.

| Status | Code | When |
|---|---|---|
| 400 | `invalid_body`, `missing_fields` | Malformed request |
| 401 | `invalid_credentials` | Wrong password or unknown user (indistinguishable) |
| 401 | `invalid_refresh_token` | Refresh cookie missing, expired, revoked or reused |
| 401 | `unauthenticated`, `invalid_token` | Missing or bad access token |
| 403 | `password_reset_required` | Legacy account whose password hash we can't verify |
| 403 | `account_inactive` | Deactivated user (only after the password checks out) |
| 429 | `too_many_attempts` | Throttled; see `Retry-After` |
| 400 | `invalid_code`, `code_expired` | Wrong or expired SMS code. A code lasts 15 minutes and allows 5 tries |
| 429 | `too_many_codes` | More than 5 codes texted to one account in an hour |

## Behaviour carried over from the legacy app

- **Identity is the phone number** (Ion Auth `identity = 'phone'`). We also accept
  email. Phones are normalised to E.164 using `LEGION_DEFAULT_CALLING_CODE` (254),
  so `0712 345 678`, `254712345678` and `+254712345678` are the same login.
- **Passwords:** existing `$2y$08$` bcrypt hashes verify unchanged. Users with legacy
  SHA-1 or junk hashes are flagged `password_reset_required` by the import.
- **Throttling:** 3 failed attempts per identity and IP within 10 minutes locks that
  pair for the rest of the window, the same as Ion Auth's `maximum_login_attempts` /
  `lockout_time`. A successful login clears the counter.

## Design notes

- **Access tokens** are HS256 JWTs (15 min by default), held in memory by the web
  app. They carry only the user id and a platform-admin flag. Group access is checked
  per request against `memberships`, so removing a member takes effect immediately
  and there is no "current group" baked into the token. The web app switches groups
  by changing which `/v1/groups/{id}/...` URLs it calls.
- **Refresh tokens** are 256-bit random values. Only their SHA-256 is stored. Each
  refresh rotates the token within a family. Presenting a rotated token more than
  10 seconds after rotation counts as theft and revokes the family. The 10-second
  grace stops two browser tabs refreshing at once from logging each other out.
- **The cookie is SameSite=Strict and scoped to `/v1/auth`.** The web app should be
  served from the same site as the API (the Vite dev server proxies `/v1`), so no
  CORS configuration is needed.
- `middleware.RealIP` trusts `X-Forwarded-For`, so the API must sit behind a proxy
  that sets it. Otherwise clients could dodge the IP part of throttling.

## Codes and invites

- **SMS codes** are 6 digits on the web and 4 in the app, stored as an HMAC, one live
  code per account and purpose (verify phone, reset password). Sending a new one
  replaces the old. The texts are the legacy wording ("kindly use 123456 as the
  activation code").
- **Invites** are texted when an official adds someone to a live group, or to
  everyone added during setup when setup is complete. Someone with no password gets
  a link to `${LEGION_WEB_URL}/join/<token>`, valid for 30 days; someone who already
  has a password gets the login address. Accepting an invite also verifies the phone.
- Accounts flagged `password_reset_required` by the import get back in through
  forgot-password.
- The old API's `signup`, `new_signup`, `activate`, `resend_activation_code`,
  `new_forgot_password` and `new_reset_password` actions (the unencrypted
  `response_code` style ones) aren't carried over. The mobile app uses
  `register_user`, `forgot_password`, `validate_forgot_password_code` and
  `reset_password`, which are.
