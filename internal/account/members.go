package account

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

var (
	ErrAlreadyMember = errors.New("already a member")
	ErrEmailTaken    = errors.New("email belongs to another user")
)

// AddMember adds a person to a group by phone number. A user with that
// phone is reused; otherwise one is created who sets a password through
// their invite or a reset. A removed or suspended membership comes back.
func AddMember(ctx context.Context, q *store.Queries, groupID int64, first, last, phone string, email *string,
	roleID *int64, official bool) (int64, error) {
	uid, err := FindOrCreateUser(ctx, q, strings.TrimSpace(first), strings.TrimSpace(last), phone, email)
	if err != nil {
		return 0, err
	}
	existing, err := q.GetMembershipByUser(ctx, store.GetMembershipByUserParams{GroupID: groupID, UserID: uid})
	switch {
	case err == nil && existing.Status == store.MembershipStatusActive:
		return 0, ErrAlreadyMember
	case err == nil:
		if err := q.ReactivateMembership(ctx, existing.ID); err != nil {
			return 0, err
		}
		return existing.ID, q.UpdateMembership(ctx, store.UpdateMembershipParams{
			GroupID: groupID, ID: existing.ID, IsOfficial: &official, GroupRoleID: roleID,
		})
	case errors.Is(err, pgx.ErrNoRows):
		return q.CreateMembership(ctx, store.CreateMembershipParams{
			GroupID: groupID, UserID: uid, IsOfficial: official, GroupRoleID: roleID,
		})
	}
	return 0, err
}

// FindOrCreateUser returns the user with a phone number, creating one
// without a password if there is none.
func FindOrCreateUser(ctx context.Context, q *store.Queries, first, last, phone string, email *string) (int64, error) {
	u, err := q.GetUserByPhone(ctx, &phone)
	if err == nil {
		return u.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if email != nil {
		if _, err := q.GetUserByEmail(ctx, *email); err == nil {
			// A different person (different phone) already uses this email.
			return 0, ErrEmailTaken
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	return q.CreateUserWithoutPassword(ctx, store.CreateUserWithoutPasswordParams{
		FirstName: first, LastName: last, Phone: &phone, Email: email,
	})
}
