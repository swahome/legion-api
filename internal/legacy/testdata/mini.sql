-- phpMyAdmin SQL Dump (trimmed fixture)
CREATE TABLE `users` (`id` int(11) NOT NULL);

INSERT INTO `users` (`id`, `password`, `email`, `phone`, `calling_code`, `first_name`, `middle_name`, `last_name`, `active`, `created_on`, `last_login`) VALUES
(10, '$2y$08$JMphu/bpdIY2rYLy27QO.u1RmzHzL892Ga1LSxjvZHDdzqEcmQAeS', 'jane@example.com', 712345678, '+254', 'Jane', NULL, 'O\'Neil', 1, 1381140600, 1664695086),
(11, 'da39a3ee5e6b4b0d3255bfef95601890afd80709', 'dup@example.com', 712345678, '254', 'Dup', '', 'User', NULL, 1381140600, NULL),
(12, NULL, 'JANE@example.com', NULL, NULL, 'Ghost', NULL, 'User', 1, NULL, NULL);

INSERT INTO `users_groups` (`id`, `user_id`, `group_id`) VALUES
(1, 10, 1),
(2, 10, 2),
(3, 11, 99);

INSERT INTO `investment_groups` (`id`, `name`, `slug`, `active`, `owner`, `created_on`, `enable_e_wallet`) VALUES
(15, 'Jirani Welfare Group', 'jirani-15', 1, 10, 1691050624, 1),
(16, 'Second, Group; Ltd', NULL, 0, 999, NULL, NULL);

INSERT INTO `group_roles` (`id`, `name`, `group_id`, `active`) VALUES
(5, 'Treasurer', 15, 1),
(6, 'Chair', 16, 1);

INSERT INTO `members` (`id`, `user_id`, `group_id`, `is_admin`, `active`, `group_role_id`, `is_deleted`, `created_on`, `occupation`) VALUES
(100, 10, 15, 1, 1, 5, NULL, 1380890384, 'Farmer'),
(101, 11, 15, 0, 1, 6, NULL, 1380890384, NULL),
(102, 11, 15, 0, 0, NULL, '1', 1380890384, NULL),
(103, 999, 15, 0, 1, NULL, NULL, NULL, NULL);

INSERT INTO `contributions` (`id`, `type`, `name`, `amount`, `group_id`, `regular_invoicing_active`, `active`, `category`, `is_deleted`) VALUES
(44, 1, 'Monthly Savings', '4200.00', 15, 1, 1, '2', NULL),
(45, 2, 'Entry fee', '500.00', 15, 0, 1, NULL, NULL);

INSERT INTO `regular_contribution_settings` (`id`, `contribution_id`, `contribution_frequency`, `month_day_monthly`, `invoice_date`, `contribution_date`, `invoice_days`, `enable_contribution_member_list`, `enable_fines`) VALUES
(1, 44, 1, '5', 1690000000, 1690400000, 3, 1, 1),
(2, 999, 1, NULL, NULL, NULL, NULL, 0, 0);

INSERT INTO `one_time_contribution_settings` (`id`, `contribution_id`, `invoice_date`, `contribution_date`) VALUES
(1, 45, 1690000000, 1690400000);

INSERT INTO `contribution_member_pairings` (`id`, `contribution_id`, `member_id`, `is_deleted`) VALUES
(1, 44, 100, NULL),
(2, 44, 101, 1),
(3, 44, 555, NULL);

INSERT INTO `contribution_fine_settings` (`id`, `contribution_id`, `fine_type`, `fixed_amount`, `fixed_fine_chargeable_on`, `percentage_rate`, `active`) VALUES
(1, 44, 1, 200, '7', NULL, 1),
(2, 44, 2, NULL, NULL, '2.5', 1),
(3, 44, 2, NULL, NULL, NULL, 1);

INSERT INTO `statements` (`id`, `transaction_type`, `transaction_date`, `contribution_id`, `member_id`, `group_id`, `amount`, `active`, `created_on`, `description`, `is_a_back_dating_record`, `deposit_id`, `fine_category_id`) VALUES
(1, 1, 1690400000, 44, 100, 15, '4200.00', 1, 1690400000, 'Invoice; July', '0', NULL, NULL),
(2, 9, 1690500000, 44, 100, 15, '3000.50', 1, 1690500000, NULL, '0', 500, NULL),
(3, 25, 1690600000, 44, 100, 15, '100.00', 1, 1690600000, NULL, '1', NULL, NULL),
(4, 26, 1690600000, 44, 100, 15, '50.00', 1, 1690600000, NULL, '0', NULL, NULL),
(5, 9, 1690700000, 44, 100, 15, '999.00', 0, 1690700000, 'voided', '0', NULL, NULL),
(6, 3, 1690700000, NULL, 100, 15, '200.00', 1, 1690700000, NULL, '0', NULL, 7),
(7, 1, 1690400000, 44, 777, 15, '4200.00', 1, 1690400000, NULL, '0', NULL, NULL),
(8, 99, 1690400000, 44, 100, 15, '1.00', 1, 1690400000, NULL, '0', NULL, NULL);

INSERT INTO `banks` (`id`, `name`, `slug`, `active`) VALUES
(1, 'Equity Bank', 'equity-bank', 1);

INSERT INTO `bank_accounts` (`id`, `bank_id`, `bank_branch_id`, `group_id`, `account_name`, `account_number`, `initial_balance`, `is_default`, `active`, `created_on`) VALUES
(1, 1, 999, 15, 'Main', 2147483647, '1000', '1', 1, 1690000000),
(2, 1, NULL, 15, 'Projects', 12345, NULL, NULL, 1, 1690000000),
(3, 1, NULL, 99, 'Orphan', 1, NULL, NULL, 1, 1690000000);

INSERT INTO `petty_cash_accounts` (`id`, `account_name`, `group_id`, `initial_balance`, `active`, `is_deleted`) VALUES
(1, 'Petty cash', 15, 0, 1, NULL);

INSERT INTO `fine_categories` (`id`, `name`, `group_id`, `amount`, `active`) VALUES
(7, 'Lateness', 15, '200.00', 1);

INSERT INTO `deposits` (`id`, `type`, `account_id`, `group_id`, `deposit_date`, `member_id`, `deposit_method`, `amount`, `active`, `contribution_id`, `fine_category_id`, `loan_id`, `created_on`) VALUES
(500, 1, 'bank-2', 15, 1690500000, 100, 1, '3000.50', 1, 44, NULL, NULL, 1690500000),
(501, 8, 0, 15, 1690700000, 100, 2, '200.00', 1, NULL, 7, NULL, 1690700000),
(502, 17, 0, 15, 1690700000, 100, 1, '50.00', 1, NULL, NULL, 31, 1690700000),
(503, 99, 0, 15, 1690700000, 100, 1, '1.00', 1, NULL, NULL, NULL, 1690700000);

INSERT INTO `withdrawals` (`id`, `type`, `account_id`, `group_id`, `withdrawal_date`, `withdrawal_method`, `amount`, `active`, `created_on`) VALUES
(600, 1, 'bank-1', 15, 1690800000, 1, '400.00', 1, 1690800000);

INSERT INTO `transaction_statements` (`id`, `transaction_type`, `transaction_date`, `account_id`, `deposit_id`, `withdrawal_id`, `group_id`, `member_id`, `amount`, `active`, `created_on`) VALUES
(700, 1, 1690500000, 'bank-2', 500, NULL, 15, 100, '3000.50', 1, 1690500000),
(701, 8, 1690700000, 0, 501, NULL, 15, 100, '200.00', 1, 1690700000),
(702, 17, 1690700000, 0, 502, NULL, 15, 100, '50.00', 1, 1690700000),
(703, 41, 1690800000, 'bank-1', NULL, 600, 15, NULL, '400.00', 1, 1690800000),
(704, 41, 1690800000, 0, NULL, 600, 15, NULL, '9.00', 0, 1690800000);
