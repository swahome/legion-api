# Groups, members and contributions

Every route below is under `/v1/groups/{groupID}` and needs a Bearer token.

## Access

| Caller | Access |
|---|---|
| Active member | Read the group, the member directory, contributions, and their own details and statement |
| Official (`memberships.is_official`) | All of the above for every member, plus the writes marked "official" |
| Platform admin who isn't a member | The same as an official. This replaces the legacy "view group as admin" |
| Anyone else, including suspended or removed members | `404 group_not_found`, so group ids can't be probed |

Access is checked against `memberships` on every request. Demoting or removing
someone takes effect immediately, without waiting for their token to expire.

## Endpoints

| Method | Path | Who | Notes |
|---|---|---|---|
| GET | `/` | member | Group details, the caller's membership and role, and a summary (active members, officials, active contributions, total contributions paid). Contact details are for officials only |
| GET | `/roles` | member | Offices in the group (Chair Person, Treasurer, ...) |
| GET | `/members?status=&q=` | member | Directory. `status` is `active` (the default) or, for officials, `suspended`, `removed` or `all`. `q` searches name, phone and membership number. Members see others' names and offices but not their phone or email. Returns 403 for members when the group set the legacy `disable_member_directory` flag |
| GET | `/members/{id}` | self or official | Full member details |
| POST | `/members` | official | Adds a person by phone. An existing user with that phone is reused. Otherwise a user is created who must reset their password before logging in. Re-adding a suspended or removed member reactivates them. 409 `already_member` or `email_taken` |
| PATCH | `/members/{id}` | official | `is_official`, `role_id` (null clears it), `membership_number`, `status`. 409 `last_official` if the change would leave no active official |
| GET | `/members/{id}/statement?from=&to=&contribution_id=` | self or official | Contribution statement with debit, credit and running balance per entry, opening and closing balance for the window, and lifetime totals per contribution. Contributions marked `hidden_in_statement` are shown to officials only |
| GET | `/contributions` | member | Hidden contributions are for officials only |
| GET | `/contributions/{id}` | member | Includes the schedule, fine rules and member list (when it doesn't apply to everyone) |
| POST | `/contributions` | official | `name`, `kind` (`regular`, `one_time` or `non_scheduled`), `amount`, optional `category`, `applies_to_all_members`, `member_ids`, `schedule`. Regular contributions need `schedule.frequency` and `schedule.contribution_date` |
| PATCH | `/contributions/{id}` | official | Any field except `kind`. `schedule` and `member_ids` replace the existing ones when sent |
| GET | `/contribution-balances` | official | Every active member's payable, paid and balance, most in arrears first |

## Conventions

- Money is a decimal string with two places, such as `"4200.00"`. Requests accept a
  string or a number with at most two decimals.
- Dates are `YYYY-MM-DD` calendar dates in the group's timezone (Africa/Nairobi).
- A positive balance means the member owes the group. A negative one means they're
  ahead.
- The `schedule.rule` object carries the legacy day and week picker settings. It's
  read-only until the invoicing engine is ported.
