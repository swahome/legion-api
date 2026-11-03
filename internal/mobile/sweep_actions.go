package mobile

import "context"

// addSweepActions registers milestone 8: the rest of the legacy app's
// actions. See docs/mobile/legacy-mobile-api.md for the ones deliberately
// not carried over.
func (s *Server) addSweepActions() {
	g := func(fn groupFn) action { return action{fn: s.groupAction(6, fn)} }
	u := func(fn func(context.Context, *call) (any, error)) action { return action{fn: s.userAction(fn)} }
	off := func(fn groupFn) action { return action{fn: s.groupAction(6, s.officialsOnly(fn))} }
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
	} {
		if _, dup := s.actions[name]; dup {
			panic("mobile: action registered twice: " + name)
		}
		s.actions[name] = a
	}
}
