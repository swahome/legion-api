# USSD and WhatsApp (milestone 7)

Members use their group from a basic phone (USSD, through Africa's Talking)
or from WhatsApp (through Twilio, as the legacy bot did). Both channels run
the same menus from `internal/channels`, and every action goes through the
same rules as the web and the app: payments through `internal/payments`,
withdrawal requests, approvals and loan applications through `internal/books`.

## Setting it up

1. Run `legion-api migrate` (migration 00015 adds `channel_sessions` and
   `channel_messages`).
2. Run `legion-api hook-urls` and copy the two lines it prints:
   - **USSD:** in the Africa's Talking dashboard, set the USSD service code's
     callback URL to the `.../v1/hooks/ussd?key=...` line.
   - **WhatsApp:** in the Twilio console, set the WhatsApp sender's "When a
     message comes in" webhook (HTTP POST) to the `.../v1/hooks/whatsapp?key=...`
     line.
3. Optional settings in `.env`:

| Setting | What it does |
|---|---|
| `LEGION_TWILIO_AUTH_TOKEN` | Twilio auth token. When set, every WhatsApp message must carry a valid `X-Twilio-Signature`, so nobody else can post as a member. Recommended. |
| `LEGION_USSD_IPS` | Africa's Talking's source IPs, comma separated. When set, USSD calls from other addresses are refused. |
| `LEGION_BRAND` | The name callers see ("Thank you for using Legion"). Default `Legion`. |
| `LEGION_PUBLIC_URL` | The address Twilio calls; it is part of the signature, so it must match the webhook URL exactly. |

The key in each URL is derived from `LEGION_HOOK_KEY` (as the payment hooks
are); changing that key changes the URLs.

Platform admins can switch either channel off without a deploy: **Admin →
USSD and WhatsApp**, or the settings `channels.ussd.enabled` and
`channels.whatsapp.enabled` (true/false). A switched-off channel answers "This
service is not available right now".

## What callers can do

Callers are recognised by phone number (the telco or WhatsApp vouches for
it), like the legacy menus; there is no PIN. Only active members of active
groups see a group. With one group the caller goes straight to its menu.

| Menu | Who | Notes |
|---|---|---|
| My account | everyone | contributions paid and owed, fines, loan balances, group account balances (cash at bank, cash at hand, total), last five payments |
| Make a payment | everyone | contribution, fine, loan repayment or other payment, by MPesa prompt to the caller's own phone. The prompt is sent three seconds after the USSD screen closes, because a phone cannot show it while USSD is open. The member gets the usual SMS when it is paid. |
| Request a withdrawal | officials and role holders | expense, contribution refund (to a member's or any MPesa number) or transfer between accounts; goes to the signatories like a request from the web |
| Approve withdrawals | officials and role holders | approve, or decline with a reason; no password, as in the legacy menu and the app |
| Loans | everyone | apply (amount and term checked against the loan type; types that always need guarantors must be applied for in the app or on the web), see applications, see balances, guarantee or approve applications waiting on you |
| Switch group | members of more than one group | |

Navigation, on every screen: `0` back (exit on the first screen), `00` the
main menu, `98` more options when a list does not fit. On WhatsApp, "hi" or
"menu" returns to the main menu and "stop" ends the conversation.

## Reliability

The legacy controllers kept a numeric position per session and re-read the
whole answer string with string matching; the WhatsApp bot used the Twilio
account id as its session id, so all callers shared one session. The rebuild:

- **One row per conversation, locked per request.** Each request locks its
  session, applies only the answers it has not seen (Africa's Talking sends
  the whole string, `1*2*500`), saves the new place and logs the exchange in
  one transaction.
- **Retries are safe.** A request the session has already answered (the same
  USSD text, or the same Twilio `MessageSid`) gets the stored reply and does
  nothing again, so a telco retry cannot send two MPesa prompts or approve
  twice. A USSD session that has ended stays ended.
- **Failures are contained.** A refused answer (wrong amount, not enough
  money, closed request) is shown above the same screen. An unexpected error
  rolls the request back, ends the session with a polite message and is
  recorded in the transcript for support. Money actions run in savepoints.
- **Screens fit.** USSD screens are laid out to 182 characters; long lists
  page with `98`.
- **Housekeeping.** Quiet conversations close (USSD after 5 minutes, WhatsApp
  after 30); conversations older than 180 days are deleted.

## Admin API

| Method | Path | |
|---|---|---|
| GET | `/v1/admin/channels` | switches, whether Twilio signatures are checked, last 7 days' use |
| GET | `/v1/admin/channel-sessions?channel=&phone=&errors=true&page=` | conversations, newest first |
| GET | `/v1/admin/channel-sessions/{id}` | one conversation with every exchange |
| POST | `/v1/admin/channels/try` | test phone: `{"format": "ussd" or "whatsapp", "session": "", "input": ""}`. Runs the real menus as the admin's own phone number, so what it does is real. Send back the `session` it returns. |

## Not carried over

- The legacy "merry-go-round" withdrawal is a contribution refund, as in the
  app; loan disbursement from USSD is left to officials on the web.
- Deleting a loan application from USSD (the legacy code had it but no menu
  reached it).
- The leftover "Insurance Regulatory Authority" demo menu in `Ussd.php`.
- The legacy `ussd` and `ussd_history` tables: sessions are short-lived and
  are not imported.
