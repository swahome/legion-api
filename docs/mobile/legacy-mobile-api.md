# Legacy mobile app API

The existing mobile app has to keep working against the new API. This page lists everything that app can call on the old PHP backend. It also describes how requests are authenticated and encrypted, and which milestone ports each module. Only the base path may change; request and response shapes stay the same.

Source: `core/Mobile_Controller.php`, `helpers/custom_helper.php` (`encrypt_json_encode`), `libraries/Encryptdecrypt.php`, `config/routes.php` and every `Mobile.php` controller in the legacy zip. The endpoint list was generated from those controllers.

## Routing

All calls are `POST` with a JSON body.

- `mobile/<action>` goes to `controllers/Mobile.php`.
- `mobile/<module>/<action>` goes to `modules/<module>/controllers/Mobile.php`.
- `mobile/<module>` goes to that module's `index`.
- An unknown action returns HTTP 404 with `{"response":{"status":0,"error":"404 Method Not Found for URI: …"}}`.

The new API will serve the same paths under a new prefix (proposed: `/mobile/v1/…`). The app then only changes its base URL.

## Headers

| Header | Meaning |
| --- | --- |
| `Authorization` | `Bearer <access token>`. Not needed for the login and registration actions listed below |
| `Secret` | Base64 of the request's AES key, RSA-encrypted with the server's public key |
| `Versioncode` / `VersionCode` | App build number. 73 or lower is rejected as outdated; above 77, responses drop the outer `response` wrapper |

These actions skip the token check: `mobile/index`, `generate_pin`, `verify_pin`, `resend_pin`, `login`, `get_user_invited_groups`, `accept_decline_invitation`, `get_checkin_data`, `register_user`, `forgot_password`, `validate_forgot_password_code`, `reset_password`, `encrypt_test`, `decrypt_test`.

## Payload encryption

Both directions use a hybrid scheme: a random 16-character AES key per message, sent RSA-encrypted.

**Request.** The body is `base64(AES-128-CBC(json)) + ":" + base64(iv)`. The AES key is the RSA-decrypted `Secret` header. The server decrypts that header with its private key, `assets/certificates/10.184.38.64.key`. The key string is zero-padded, and AES-128 uses its first 16 bytes.

**Response.** The body is `{"secret": base64(RSA(key, mobile.crt)), "body": base64(AES-128-CBC(json)) + ":" + base64(iv)}`. The key is a fresh random 16-character string, RSA-encrypted with the app's public certificate, `assets/certificates/mobile.crt`. The IV is `time() . "123456"`, so 16 ASCII characters.

Decrypted request JSON always carries `user_id`, `group_id` and `request_id`. With a valid token, the `user_id` in the body must match the token's user.

**Blocker.** The zip has no `assets/certificates/` folder. The new API needs the server private key `10.184.38.64.key` and the app certificate `mobile.crt` from production. Without them it can neither read the app's requests nor answer in a form the app accepts.

## Auth and status conventions

- `mobile/login` returns an `access_token`, a random 32-character string that lasts 100 days. It's stored per user (the `access_tokens` table) and checked on every call.
- `mobile/generate_pin` and `mobile/verify_pin` run the phone-number PIN login.
- Errors come back as HTTP 200 with a `status` field: `0` or `400` for failure, `2` for an empty body, `3` or `401` for an outdated app, and `1` for success in most actions.

## Not carried over

The old code has back doors that the new API will not reproduce:

- A fixed token and secret, `abcdefghhgfedcba`, skip authentication and encryption entirely.
- Requests from `127.0.0.1` or `197.237.106.185` are accepted with any token, as any user.
- Every request, including passwords and PINs, is written in plain text to `logs/app_log_user.dat` and `logs/mobile_app_payload.dat`.

If any released build of the app relies on the fixed token, we need to know before milestone 2 ships.

## Endpoints by milestone

### Milestone 2: Role panels and platform admin (112 actions)

| Path | Request fields (besides user_id, group_id, request_id) |
| --- | --- |
| `mobile/activate` |  |
| `mobile/bank_branches/get_bank_branches_options` |  |
| `mobile/banks/get_bank_options` |  |
| `mobile/change_email_address` |  |
| `mobile/change_phone_number` |  |
| `mobile/change_user_password` |  |
| `mobile/check_contribution_member_list` |  |
| `mobile/check_if_date_is_out_of_range` |  |
| `mobile/check_if_invoice_date_is_less_than_contribution_date` |  |
| `mobile/check_if_second_date_is_out_of_range` |  |
| `mobile/check_template_placeholders` |  |
| `mobile/check_user_validation_status` |  |
| `mobile/complete_group_setup` |  |
| `mobile/contributions/delete` |  |
| `mobile/contributions/get_group_contribution` |  |
| `mobile/contributions/get_group_contribution_options` |  |
| `mobile/contributions/get_group_contributions` |  |
| `mobile/contributions/get_group_contributions_to_pay` |  |
| `mobile/contributions/get_member_contribution_options` |  |
| `mobile/contributions/hide` |  |
| `mobile/contributions/unhide` |  |
| `mobile/countries/get_country_options` |  |
| `mobile/countries/get_currency_options` |  |
| `mobile/create_group` |  |
| `mobile/delete_group_member` |  |
| `mobile/forgot_password` |  |
| `mobile/generate_member_statement` |  |
| `mobile/generate_pin` |  |
| `mobile/get_all_otps` |  |
| `mobile/get_group_bank_balances` |  |
| `mobile/get_group_dashboard_data` |  |
| `mobile/get_group_data` |  |
| `mobile/get_group_member_contribution_summary` |  |
| `mobile/get_group_member_fines_summary` |  |
| `mobile/get_group_member_loans_summary` |  |
| `mobile/get_group_member_notifications_summary` |  |
| `mobile/get_group_more_dashboard_data` |  |
| `mobile/get_group_recent_transactions` |  |
| `mobile/get_ios_member_and_group_chart_data` |  |
| `mobile/get_member_and_group_chart_data` |  |
| `mobile/get_member_dashboard` |  |
| `mobile/get_member_dashboard_data` |  |
| `mobile/get_member_recent_transactions` |  |
| `mobile/get_user_checkin_data` |  |
| `mobile/get_user_group_data_information` |  |
| `mobile/group_roles/create` |  |
| `mobile/group_roles/edit` |  |
| `mobile/group_roles/get_group_unassigned_role_ids` |  |
| `mobile/group_roles/get_group_unassigned_roles` |  |
| `mobile/group_roles/group_roles_list` |  |
| `mobile/group_roles/hide` |  |
| `mobile/group_roles/unhide` |  |
| `mobile/groups/edit_new_profile_photo` |  |
| `mobile/groups/edit_profile_photo` |  |
| `mobile/groups/update_country` |  |
| `mobile/groups/update_currency` |  |
| `mobile/groups/update_email` |  |
| `mobile/groups/update_group_settings` |  |
| `mobile/groups/update_name` |  |
| `mobile/groups/update_phone` |  |
| `mobile/login` |  |
| `mobile/manage_asset_categories` |  |
| `mobile/manage_group_account` |  |
| `mobile/manage_group_contributions` |  |
| `mobile/manage_group_loan_type` |  |
| `mobile/members/activate_member` |  |
| `mobile/members/delete_member` |  |
| `mobile/members/edit_group_member` |  |
| `mobile/members/edit_profile_photo` |  |
| `mobile/members/get_group_member_options` |  |
| `mobile/members/get_members_and_details` |  |
| `mobile/members/reassign_member_group_role` |  |
| `mobile/members/send_invitation` |  |
| `mobile/members/suspend_member` |  |
| `mobile/mobile_money_providers/get_mobile_money_provider_options` |  |
| `mobile/new_forgot_password` |  |
| `mobile/new_reset_password` |  |
| `mobile/new_signup` |  |
| `mobile/notifications/create_test` |  |
| `mobile/notifications/get_all_group_member_notifications` |  |
| `mobile/notifications/get_group_member_notification_count` |  |
| `mobile/notifications/mark_all_as_read` |  |
| `mobile/notifications/mark_all_as_unread` |  |
| `mobile/notifications/mark_as_read` |  |
| `mobile/register_member` |  |
| `mobile/register_user` |  |
| `mobile/resend_activation_code` |  |
| `mobile/resend_pin` |  |
| `mobile/reset_password` |  |
| `mobile/sacco_branches/get_sacco_branches_options` |  |
| `mobile/saccos/get_sacco_options` |  |
| `mobile/save_activity_log` |  |
| `mobile/send_push_notifications` |  |
| `mobile/signup` |  |
| `mobile/statements/contribution_statement` |  |
| `mobile/statements/download_statement` |  |
| `mobile/statements/fine_statement` |  |
| `mobile/statements/send_email_statement` |  |
| `mobile/statements/total_group_savings` |  |
| `mobile/update_group_loan_setting` |  |
| `mobile/users/change_user_phone_number` |  |
| `mobile/users/edit_new_profile_photo` |  |
| `mobile/users/edit_profile_photo` |  |
| `mobile/users/resend_code` |  |
| `mobile/users/update_email` |  |
| `mobile/users/update_name` |  |
| `mobile/users/update_profile` |  |
| `mobile/users/update_user_mobile_token` |  |
| `mobile/users/verify_code` |  |
| `mobile/validate_forgot_password_code` |  |
| `mobile/validate_user` |  |
| `mobile/verify_pin` |  |

### Milestone 3: Money in and out (116 actions)

| Path | Request fields (besides user_id, group_id, request_id) |
| --- | --- |
| `mobile/accounts/get_group_account_options` |  |
| `mobile/accounts/get_group_active_account_options` |  |
| `mobile/bank_accounts/activate` |  |
| `mobile/bank_accounts/activate_wallet` |  |
| `mobile/bank_accounts/check_email_selected_members` |  |
| `mobile/bank_accounts/check_sms_selected_members` |  |
| `mobile/bank_accounts/close` |  |
| `mobile/bank_accounts/connect` |  |
| `mobile/bank_accounts/create` |  |
| `mobile/bank_accounts/create_otp` |  |
| `mobile/bank_accounts/delete` |  |
| `mobile/bank_accounts/disconnect` |  |
| `mobile/bank_accounts/disconnect_account` |  |
| `mobile/bank_accounts/edit` |  |
| `mobile/bank_accounts/get_group_bank_accounts_list` |  |
| `mobile/bank_accounts/verify` |  |
| `mobile/bank_accounts/verify_account` |  |
| `mobile/bank_accounts/verify_otp` |  |
| `mobile/depositors/create` |  |
| `mobile/depositors/get_group_depositor_options` |  |
| `mobile/deposits/calculate_convenience_charge` |  |
| `mobile/deposits/get_deposits_list` |  |
| `mobile/deposits/get_group_contribution_transfers` |  |
| `mobile/deposits/get_group_deposit` |  |
| `mobile/deposits/make_group_arrears_payment` |  |
| `mobile/deposits/make_group_payment_old_old` |  |
| `mobile/deposits/make_new_group_payment` |  |
| `mobile/deposits/make_new_group_payment_copy` |  |
| `mobile/deposits/make_new_group_payment_old` |  |
| `mobile/deposits/make_payment_to_group_bank_express` |  |
| `mobile/deposits/make_payment_to_group_bank_express_old` |  |
| `mobile/deposits/new_record_contribution_payments` |  |
| `mobile/deposits/new_record_fine_payments` |  |
| `mobile/deposits/new_record_income_payment` |  |
| `mobile/deposits/new_record_miscellaneous_payments` |  |
| `mobile/deposits/record_bank_loan` |  |
| `mobile/deposits/record_contribution_payments` |  |
| `mobile/deposits/record_contribution_transfer` |  |
| `mobile/deposits/record_fine_payments` |  |
| `mobile/deposits/record_income` |  |
| `mobile/deposits/record_loan_repayments` |  |
| `mobile/deposits/record_miscellaneous_payments` |  |
| `mobile/deposits/void` |  |
| `mobile/deposits/void_bank_loan` |  |
| `mobile/deposits/void_contribution_transfer` |  |
| `mobile/expense_categories/create` |  |
| `mobile/expense_categories/delete` |  |
| `mobile/expense_categories/edit` |  |
| `mobile/expense_categories/get_group_expense_categories` |  |
| `mobile/expense_categories/get_group_expense_category_options` |  |
| `mobile/expense_categories/hide` |  |
| `mobile/expense_categories/manage` |  |
| `mobile/expense_categories/unhide` |  |
| `mobile/fine_categories/create` |  |
| `mobile/fine_categories/delete` |  |
| `mobile/fine_categories/edit` |  |
| `mobile/fine_categories/get_group_fine_category_options` |  |
| `mobile/fine_categories/group_fine_categories_list` |  |
| `mobile/fine_categories/hide` |  |
| `mobile/fine_categories/unhide` |  |
| `mobile/fines/fine_members` |  |
| `mobile/fines/get_group_fine_options` |  |
| `mobile/fines/get_group_fines_list` |  |
| `mobile/fines/new_fine_members` |  |
| `mobile/income_categories/create` |  |
| `mobile/income_categories/delete` |  |
| `mobile/income_categories/edit` |  |
| `mobile/income_categories/get_group_income_category_options` |  |
| `mobile/income_categories/group_income_categories_list` |  |
| `mobile/income_categories/hide` |  |
| `mobile/income_categories/index` |  |
| `mobile/income_categories/manage` |  |
| `mobile/income_categories/unhide` |  |
| `mobile/invoices/create` | member_ids |
| `mobile/invoices/get_group_invoice` |  |
| `mobile/invoices/get_group_invoices` |  |
| `mobile/invoices/void` |  |
| `mobile/mobile_money_accounts/activate` |  |
| `mobile/mobile_money_accounts/close` |  |
| `mobile/mobile_money_accounts/create` |  |
| `mobile/mobile_money_accounts/delete` |  |
| `mobile/mobile_money_accounts/edit` |  |
| `mobile/mobile_money_accounts/get_group_mobile_money_accounts_list` |  |
| `mobile/petty_cash_accounts/activate` |  |
| `mobile/petty_cash_accounts/close` |  |
| `mobile/petty_cash_accounts/create` |  |
| `mobile/petty_cash_accounts/delete` |  |
| `mobile/petty_cash_accounts/edit` |  |
| `mobile/petty_cash_accounts/get_group_petty_cash_accounts_list` |  |
| `mobile/recipients/create` |  |
| `mobile/reports/account_balances` |  |
| `mobile/reports/bank_loans_summary` |  |
| `mobile/reports/contribution_summary` |  |
| `mobile/reports/expenses_summary` |  |
| `mobile/reports/fines_summary` |  |
| `mobile/reports/loans_summary` |  |
| `mobile/reports/transaction_statement` |  |
| `mobile/sacco_accounts/activate` |  |
| `mobile/sacco_accounts/close` |  |
| `mobile/sacco_accounts/create` |  |
| `mobile/sacco_accounts/delete` |  |
| `mobile/sacco_accounts/edit` |  |
| `mobile/sacco_accounts/get_group_sacco_accounts_list` |  |
| `mobile/withdrawals/cancel_withdrawal_request` |  |
| `mobile/withdrawals/get_group_withdrawal` |  |
| `mobile/withdrawals/get_group_withdrawal_list` |  |
| `mobile/withdrawals/new_record_expenses` |  |
| `mobile/withdrawals/record_bank_loan_repayment` |  |
| `mobile/withdrawals/record_contribution_refund` |  |
| `mobile/withdrawals/record_expenses` |  |
| `mobile/withdrawals/record_funds_transfer` |  |
| `mobile/withdrawals/request_funds_transfer` |  |
| `mobile/withdrawals/respond_to_withdrawal_request` |  |
| `mobile/withdrawals/view_withdrawal_request` |  |
| `mobile/withdrawals/void` |  |
| `mobile/withdrawals/withdrawal_request_list` |  |

### Milestone 4: Loans (37 actions)

| Path | Request fields (besides user_id, group_id, request_id) |
| --- | --- |
| `mobile/bank_loans/get_group_bank_loan_options` |  |
| `mobile/loan_applications/approve_loan_application` |  |
| `mobile/loan_applications/approve_loan_guarantorship_request` |  |
| `mobile/loan_applications/approve_ngo_loan_application` |  |
| `mobile/loan_applications/create_amt_loan_application` |  |
| `mobile/loan_applications/create_group_loan_application` |  |
| `mobile/loan_applications/decline_loan_guarantorship_request` |  |
| `mobile/loan_applications/get_group_loan_applications` |  |
| `mobile/loan_applications/get_group_loan_calculator` |  |
| `mobile/loan_applications/get_loan_application_approval_requests` |  |
| `mobile/loan_applications/get_loan_application_guarantorship_requests` |  |
| `mobile/loan_applications/get_member_action_approval_requests` |  |
| `mobile/loan_applications/get_member_action_guarantorship_requests` |  |
| `mobile/loan_applications/get_member_loan_applications` |  |
| `mobile/loan_applications/toggle_loan_application_status` |  |
| `mobile/loan_applications/upload_document` |  |
| `mobile/loan_applications/void_group_loan_application` |  |
| `mobile/loan_types/create` |  |
| `mobile/loan_types/delete` |  |
| `mobile/loan_types/edit` |  |
| `mobile/loan_types/get_group_loan_type` |  |
| `mobile/loan_types/get_group_loan_type_application_options` |  |
| `mobile/loan_types/get_group_loan_type_options` |  |
| `mobile/loan_types/get_group_loan_types_list` |  |
| `mobile/loan_types/hide` |  |
| `mobile/loan_types/unhide` |  |
| `mobile/loan_types/update_general_details` |  |
| `mobile/loan_types/update_loan_type_fines` |  |
| `mobile/loans/edit_member_loan` |  |
| `mobile/loans/get_group_loan` |  |
| `mobile/loans/get_group_loans_list` |  |
| `mobile/loans/get_member_loan_calculator` |  |
| `mobile/loans/get_member_loan_options` |  |
| `mobile/loans/get_members_loans_options` |  |
| `mobile/loans/record_member_loan` |  |
| `mobile/loans/statement` |  |
| `mobile/loans/void` |  |

### Milestone 5: Sign-up, SMS and the setup wizard (9 actions)

| Path | Request fields (besides user_id, group_id, request_id) |
| --- | --- |
| `mobile/setup_tasks/add_group_members` |  |
| `mobile/setup_tasks/cancel_group_invitation` |  |
| `mobile/setup_tasks/contributing_members` |  |
| `mobile/setup_tasks/contribution_fine_settings` |  |
| `mobile/setup_tasks/create_group_contribution_setting` |  |
| `mobile/setup_tasks/edit_contribution_setting` |  |
| `mobile/setup_tasks/index` |  |
| `mobile/setup_tasks/reassign_member_group_role` |  |
| `mobile/setup_tasks/resend_group_invitation` |  |

### Milestone 6: Payments and reconciliation (5 actions)

| Path | Request fields (besides user_id, group_id, request_id) |
| --- | --- |
| `mobile/transaction_alerts/get_group_unreconcilled_deposits` |  |
| `mobile/transaction_alerts/get_group_unreconcilled_withdrawals` |  |
| `mobile/transaction_alerts/payment_request` |  |
| `mobile/transaction_alerts/reconcile_deposits` |  |
| `mobile/transaction_alerts/reconcile_withdrawals` |  |

### Milestone 8: Mobile app API (remaining modules) (47 actions)

| Path | Request fields (besides user_id, group_id, request_id) |
| --- | --- |
| `mobile/asset_categories/create` |  |
| `mobile/asset_categories/edit` |  |
| `mobile/asset_categories/get_group_asset_categories` |  |
| `mobile/asset_categories/hide` |  |
| `mobile/asset_categories/unhide` |  |
| `mobile/assets/asset_purchase_payments` |  |
| `mobile/assets/asset_sales_list` |  |
| `mobile/assets/create_asset` |  |
| `mobile/assets/get_group_asset_options` |  |
| `mobile/assets/get_group_assets` |  |
| `mobile/assets/record_asset_purchase_payments` |  |
| `mobile/assets/sell` |  |
| `mobile/assets/void` |  |
| `mobile/billing/calculate_conversion` |  |
| `mobile/billing/confirm_free_billing` |  |
| `mobile/billing/get_group_amount_payable` |  |
| `mobile/billing/get_group_amount_payables` |  |
| `mobile/billing/get_group_billing_invoices` |  |
| `mobile/billing/get_group_billing_payments` |  |
| `mobile/billing/make_mpesa_payment` |  |
| `mobile/billing/receive_complete_billing` |  |
| `mobile/debtor_loan_types/create` |  |
| `mobile/debtor_loan_types/delete` |  |
| `mobile/debtor_loan_types/edit` |  |
| `mobile/debtor_loan_types/get_group_loan_type` |  |
| `mobile/debtor_loan_types/get_group_loan_type_application_options` |  |
| `mobile/debtor_loan_types/get_group_loan_type_options` |  |
| `mobile/debtor_loan_types/get_group_loan_types_list` |  |
| `mobile/debtor_loan_types/hide` |  |
| `mobile/debtor_loan_types/unhide` |  |
| `mobile/debtor_loan_types/update_general_details` |  |
| `mobile/debtor_loan_types/update_loan_type_fines` |  |
| `mobile/debtors/get_debtors_list` |  |
| `mobile/meetings/create_group_meeting` |  |
| `mobile/meetings/get_group_meetings` |  |
| `mobile/money_market_investments/cashin` |  |
| `mobile/money_market_investments/create` |  |
| `mobile/money_market_investments/get_money_market_investment_cashins` |  |
| `mobile/money_market_investments/get_money_market_investment_list` |  |
| `mobile/money_market_investments/topup` |  |
| `mobile/money_market_investments/void` |  |
| `mobile/stocks/get_group_stock_sales_list` |  |
| `mobile/stocks/get_group_stocks_list` |  |
| `mobile/stocks/record_stock_purchase` |  |
| `mobile/stocks/sell_stocks` |  |
| `mobile/stocks/update_stock_current_price` |  |
| `mobile/stocks/void` |  |

