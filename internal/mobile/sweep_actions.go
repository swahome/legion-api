package mobile

import "context"

// addSweepActions registers milestone 8: the rest of the legacy app's
// actions. See docs/mobile/legacy-mobile-api.md for the ones deliberately
// not carried over.
func (s *Server) addSweepActions() {
	g := func(fn groupFn) action { return action{fn: s.groupAction(6, fn)} }
	u := func(fn func(context.Context, *call) (any, error)) action { return action{fn: s.userAction(fn)} }
	off := func(fn groupFn) action { return action{fn: s.groupAction(6, s.officialsOnly(fn))} }
	active := func(fn groupFn) action { return action{fn: s.groupAction(0, s.activeOnly(fn))} }
	for name, a := range map[string]action{
		// Assets.
		"asset_categories/get_group_asset_categories": g(s.getGroupAssetCategories),
		"asset_categories/create":                     g(s.saveAssetCategory(false)),
		"asset_categories/edit":                       g(s.saveAssetCategory(true)),
		"asset_categories/hide":                       g(s.hideAssetCategory(true)),
		"asset_categories/unhide":                     g(s.hideAssetCategory(false)),
		"assets/create_asset":                         off(s.createAsset),
		"assets/get_group_assets":                     g(s.getGroupAssets),
		"assets/get_group_asset_options":              g(s.getGroupAssetOptions),
		"assets/record_asset_purchase_payments":       off(s.recordAssetPurchasePayment),
		"assets/sell":                                 off(s.sellAsset),
		"assets/void":                                 off(s.voidAsset),
		"assets/asset_purchase_payments":              g(s.assetPurchasePayments),
		"assets/asset_sales_list":                     g(s.assetSalesList),

		// Stocks.
		"stocks/record_stock_purchase":      off(s.recordStockPurchase),
		"stocks/update_stock_current_price": off(s.updateStockCurrentPrice),
		"stocks/sell_stocks":                off(s.sellStocks),
		"stocks/void":                       off(s.voidStock),
		"stocks/get_group_stocks_list":      g(s.getGroupStocksList),
		"stocks/get_group_stock_sales_list": g(s.getGroupStockSalesList),

		// Money market investments.
		"money_market_investments/create":                              off(s.createMoneyMarketInvestment),
		"money_market_investments/topup":                               off(s.topUpMoneyMarketInvestment),
		"money_market_investments/cashin":                              off(s.cashInMoneyMarketInvestment),
		"money_market_investments/void":                                off(s.voidMoneyMarketInvestment),
		"money_market_investments/get_money_market_investment_list":    g(s.getMoneyMarketInvestmentList),
		"money_market_investments/get_money_market_investment_cashins": g(s.getMoneyMarketInvestmentCashins),

		// Meetings.
		"meetings/create_group_meeting": off(s.createGroupMeeting),
		"meetings/get_group_meetings":   g(s.getGroupMeetings),

		// Contribution transfers.
		"deposits/record_contribution_transfer":     off(s.recordContributionTransfer),
		"deposits/void_contribution_transfer":       off(s.voidContributionTransfer),
		"deposits/get_group_contribution_transfers": g(s.getGroupContributionTransfers),

		// Notifications.
		"notifications/get_all_group_member_notifications":  g(s.getAllGroupMemberNotifications),
		"notifications/get_group_member_notification_count": g(s.getGroupMemberNotificationCount),
		"notifications/mark_all_as_read":                    g(s.markAllNotifications(true)),
		"notifications/mark_all_as_unread":                  g(s.markAllNotifications(false)),
		"notifications/mark_as_read":                        g(s.markNotificationRead),
		"get_group_member_notifications_summary":            {fn: s.groupAction(0, s.activeOnly(s.getGroupMemberNotificationsSummary))},

		// The user's own profile.
		"users/update_name":               u(s.updateName),
		"users/update_email":              u(s.updateEmail),
		"users/update_profile":            u(s.updateProfile),
		"users/change_user_phone_number":  u(s.changeUserPhoneNumber),
		"users/resend_code":               u(s.resendPhoneCode),
		"users/verify_code":               u(s.verifyPhoneCode),
		"users/update_user_mobile_token":  u(s.updateUserMobileToken),
		"users/edit_new_profile_photo":    u(s.editNewProfilePhoto),
		"users/edit_profile_photo":        u(s.editProfilePhoto),
		"change_user_password":            u(s.changeUserPassword),
		"check_user_validation_status":    u(s.checkUserValidationStatus),
		"validate_user":                   u(s.validateUser),
		"get_user_group_data_information": u(s.getUserGroupDataInformation),
		"save_activity_log":               u(s.saveActivityLog),

		// The group's profile and settings.
		"groups/update_name":            off(s.updateGroupName),
		"groups/update_phone":           off(s.updateGroupPhone),
		"groups/update_email":           off(s.updateGroupEmail),
		"groups/update_country":         off(s.updateGroupCountry(false)),
		"groups/update_currency":        off(s.updateGroupCountry(true)),
		"groups/update_group_settings":  off(s.updateGroupSettings),
		"groups/edit_new_profile_photo": off(s.editNewGroupProfilePhoto),
		"groups/edit_profile_photo":     off(s.editGroupProfilePhoto),

		// Roles and members.
		"group_roles/create":                        off(s.saveGroupRole(false)),
		"group_roles/edit":                          off(s.saveGroupRole(true)),
		"group_roles/hide":                          off(s.hideGroupRole(true)),
		"group_roles/unhide":                        off(s.hideGroupRole(false)),
		"group_roles/get_group_unassigned_roles":    g(s.unassignedRoles(false)),
		"group_roles/get_group_unassigned_role_ids": g(s.unassignedRoles(true)),
		"members/suspend_member":                    off(s.suspendMember(true)),
		"members/activate_member":                   off(s.suspendMember(false)),
		"members/delete_member":                     off(s.deleteMember),
		"members/edit_group_member":                 off(s.editGroupMember),
		"members/edit_profile_photo":                g(s.editMemberProfilePhoto),

		// Contributions.
		"contributions/hide":                           off(s.hideContribution(true)),
		"contributions/unhide":                         off(s.hideContribution(false)),
		"contributions/delete":                         off(s.deleteContribution),
		"contributions/get_group_contributions_to_pay": g(s.getGroupContributionsToPay),

		// Dashboards, charts and reports.
		"get_member_dashboard":                active(s.getMemberDashboard),
		"get_group_more_dashboard_data":       active(s.getGroupMoreDashboardData),
		"get_group_bank_balances":             active(s.getGroupBankBalances),
		"get_group_member_fines_summary":      active(s.getGroupMemberFinesSummary),
		"get_member_and_group_chart_data":     active(s.memberAndGroupChart(false)),
		"get_ios_member_and_group_chart_data": active(s.memberAndGroupChart(true)),
		"reports/account_balances":            {fn: s.groupAction(6, s.accountBalances)},
		"statements/fine_statement":           {fn: s.groupAction(0, s.fineStatement)},
		"statements/total_group_savings":      {fn: s.groupAction(0, s.totalGroupSavings)},
		"statements/download_statement":       {fn: s.groupAction(0, s.requestStatement(statementByDownload))},
		"statements/send_email_statement":     {fn: s.groupAction(0, s.requestStatement(statementByEmail))},

		// Subscription billing: history only, everyone on the free plan.
		"billing/get_group_billing_invoices": off(s.getGroupBillingInvoices),
		"billing/get_group_billing_payments": off(s.getGroupBillingPayments),
		"billing/get_group_amount_payables":  g(s.getGroupAmountPayable),
		"billing/get_group_amount_payable":   g(s.getGroupAmountPayable),
		"billing/make_mpesa_payment":         g(s.noBillingPayment),
		"billing/receive_complete_billing":   g(s.noBillingPayment),
		"billing/confirm_free_billing":       {fn: s.groupAction(5, s.confirmFreeBilling)},
		"billing/calculate_conversion":       u(s.calculateConversion),

		// Bank accounts linked to their bank's alerts, and payments.
		"bank_accounts/connect":                           off(s.connectBankAccount),
		"bank_accounts/verify":                            off(s.verifyBankAccount),
		"bank_accounts/disconnect":                        off(s.disconnectBankAccount),
		"bank_accounts/activate_wallet":                   off(s.activateWallet),
		"deposits/calculate_convenience_charge":           g(s.calculateConvenienceCharge),
		"deposits/make_group_payment_old_old":             g(s.makeGroupPayment),
		"deposits/make_new_group_payment_copy":            g(s.makeGroupPayment),
		"deposits/make_new_group_payment_old":             g(s.makeGroupPayment),
		"deposits/make_payment_to_group_bank_express_old": g(s.makeGroupPayment),

		// Push notifications are not sent (see notifications.go).
		"send_push_notifications":   u(s.pushNotSent),
		"notifications/create_test": u(s.pushNotSent),
	} {
		if _, dup := s.actions[name]; dup {
			panic("mobile: action registered twice: " + name)
		}
		s.actions[name] = a
	}
}
