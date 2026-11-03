package mobile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/swahome/legion-api/internal/account"
	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// The user's own profile from the app: names, email, phone (confirmed by
// a code texted to the new number), password, photo and push token, plus
// the identity check and the activity log upload.

func (s *Server) me(ctx context.Context, c *call) (store.User, error) {
	return s.q.GetUserByID(ctx, c.userID)
}

// splitName is the legacy name split: two words are first and last, three
// put two in the first name, four split two and two; one word is refused.
func splitName(name string) (first, last string, ok bool) {
	words := strings.Fields(name)
	up := func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	}
	switch len(words) {
	case 0, 1:
		return "", "", false
	case 2:
		return up(words[0]), up(words[1]), true
	case 3:
		return up(words[0] + " " + words[1]), up(words[2]), true
	case 4:
		return up(words[0] + " " + words[1]), up(words[2] + " " + words[3]), true
	}
	return up(strings.Join(words, " ")), "", true
}

func (s *Server) updateName(ctx context.Context, c *call) (any, error) {
	name := c.req.Str("name")
	if name == "" {
		return status(0, "User name is not found"), nil
	}
	first, last, ok := splitName(name)
	if !ok {
		return status(0, "Kindly enter all your usernames. You can not use one name"), nil
	}
	if err := s.q.UpdateUserNames(ctx, store.UpdateUserNamesParams{ID: c.userID, FirstName: first, LastName: last}); err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "Successful", "first_name": first, "last_name": last}, nil
}

func validEmail(v string) bool {
	at := strings.LastIndex(v, "@")
	return at > 0 && at < len(v)-3 && strings.Contains(v[at:], ".") && !strings.ContainsAny(v, " ,;")
}

func (s *Server) emailTaken(ctx context.Context, email string, userID int64) (bool, error) {
	return s.q.EmailTaken(ctx, store.EmailTakenParams{Email: email, ExceptID: userID})
}

func (s *Server) updateEmail(ctx context.Context, c *call) (any, error) {
	email := strings.ToLower(c.req.Str("email"))
	switch {
	case email == "":
		return timed(map[string]any{"status": 0, "message": "Form validation failed",
			"validation_errors": map[string]string{"email": "The Email field is required."}}, s.now()), nil
	case !validEmail(email):
		return timed(map[string]any{"status": 0, "message": "Form validation failed",
			"validation_errors": map[string]string{"email": "The Email field must contain a valid email address."}}, s.now()), nil
	}
	taken, err := s.emailTaken(ctx, email, c.userID)
	if err != nil {
		return nil, err
	}
	if taken {
		return status(0, "Email update failed: Email already in use with a different account"), nil
	}
	if err := s.q.UpdateUserEmail(ctx, store.UpdateUserEmailParams{ID: c.userID, Email: &email}); err != nil {
		return nil, err
	}
	return timed(status(1, "Success"), s.now()), nil
}

// startPhoneChange records the move and texts a code to the new number.
func (s *Server) startPhoneChange(ctx context.Context, q *store.Queries, u store.User, phone string) error {
	if err := q.PutPhoneChange(ctx, store.PutPhoneChangeParams{UserID: u.ID, OldPhone: u.Phone, NewPhone: phone}); err != nil {
		return err
	}
	return s.acct.SendCodeTo(ctx, q, u, account.ChangePhone, 4, phone)
}

func (s *Server) phoneTaken(ctx context.Context, phone string, userID int64) (bool, error) {
	return s.q.PhoneTaken(ctx, store.PhoneTakenParams{Phone: phone, ExceptID: userID})
}

// updateProfile sets names and email; a new phone starts a phone change.
func (s *Server) updateProfile(ctx context.Context, c *call) (any, error) {
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(c.req.Str("email"))
	if email != "" {
		taken, err := s.emailTaken(ctx, email, u.ID)
		if err != nil {
			return nil, err
		}
		if taken {
			return status(0, "Email update failed: Email already in use with a different account"), nil
		}
	}
	var newPhone string
	if p := c.req.Str("phone"); p != "" {
		phone, err := auth.NormalizePhone(p, s.auth.DefaultCallingCode())
		if err == nil && phone != deref(u.Phone) {
			taken, err := s.phoneTaken(ctx, phone, u.ID)
			if err != nil {
				return nil, err
			}
			if taken {
				return status(0, "Phone change failed: Phone number already in use with a different account"), nil
			}
			newPhone = phone
		}
	}
	name := c.req.Str("name")
	if name == "" {
		return status(0, "User name is not found"), nil
	}
	first, last, ok := splitName(name)
	if !ok {
		return status(0, "Kindly enter all your usernames. You can not use one name"), nil
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := q.UpdateUserNames(ctx, store.UpdateUserNamesParams{ID: u.ID, FirstName: first, LastName: last}); err != nil {
			return err
		}
		if email != "" {
			if err := q.UpdateUserEmail(ctx, store.UpdateUserEmailParams{ID: u.ID, Email: &email}); err != nil {
				return err
			}
		}
		if newPhone != "" {
			return s.startPhoneChange(ctx, q, u, newPhone)
		}
		return nil
	})
	if errors.Is(err, account.ErrTooManyCodes) {
		return status(0, "Phone number update failed: Failed to send OTP code. Try again"), nil
	}
	if err != nil {
		return nil, err
	}
	if newPhone != "" {
		s.smsKick()
	}
	return status(1, "successful"), nil
}

func (s *Server) changeUserPhoneNumber(ctx context.Context, c *call) (any, error) {
	raw := c.req.Str("new_phone")
	if raw == "" {
		return timed(map[string]any{"status": 0, "message": "Form validation failed",
			"validation_errors": map[string]string{"new_phone": "The New Phone field is required."}}, s.now()), nil
	}
	phone, err := auth.NormalizePhone(raw, s.auth.DefaultCallingCode())
	if err != nil {
		return timed(map[string]any{"status": 0, "message": "Form validation failed",
			"validation_errors": map[string]string{"new_phone": "The New Phone field must contain a valid phone number."}}, s.now()), nil
	}
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	taken, err := s.phoneTaken(ctx, phone, u.ID)
	if err != nil {
		return nil, err
	}
	if taken || phone == deref(u.Phone) {
		return status(0, "Phone change failed: Phone number already in use with a different account"), nil
	}
	err = s.inTx(ctx, func(q *store.Queries) error { return s.startPhoneChange(ctx, q, u, phone) })
	if errors.Is(err, account.ErrTooManyCodes) {
		return status(0, "Phone number update failed: Failed to send OTP code. Try again"), nil
	}
	if err != nil {
		return nil, err
	}
	s.smsKick()
	return status(1, "successful"), nil
}

func (s *Server) resendPhoneCode(ctx context.Context, c *call) (any, error) {
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	pc, err := s.q.GetPhoneChange(ctx, u.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return status(0, "Could not resend code. Kindly start change start over again"), nil
	}
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		return s.acct.SendCodeTo(ctx, q, u, account.ChangePhone, 4, pc.NewPhone)
	})
	if errors.Is(err, account.ErrTooManyCodes) {
		return status(0, "Phone number update failed: Failed to send OTP code. Try again"), nil
	}
	if err != nil {
		return nil, err
	}
	s.smsKick()
	return status(1, "successful"), nil
}

func (s *Server) verifyPhoneCode(ctx context.Context, c *call) (any, error) {
	code := c.req.Str("code")
	if code == "" {
		return timed(map[string]any{"status": 0, "message": "Form validation failed",
			"validation_errors": "Enter code sent to your phone number"}, s.now()), nil
	}
	pc, err := s.q.GetPhoneChange(ctx, c.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return timed(status(0, "Could not verify code. Try again later"), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := s.acct.CheckCode(ctx, q, c.userID, account.ChangePhone, code); err != nil {
			return err
		}
		taken, err := q.PhoneTaken(ctx, store.PhoneTakenParams{Phone: pc.NewPhone, ExceptID: c.userID})
		if err != nil {
			return err
		}
		if taken {
			reply = status(0, "That phone number is now used by another account. Kindly contact support for account merger")
			return nil
		}
		if err := q.UpdateUserPhone(ctx, store.UpdateUserPhoneParams{ID: c.userID, Phone: &pc.NewPhone}); err != nil {
			return err
		}
		return q.DeletePhoneChange(ctx, c.userID)
	})
	switch {
	case errors.Is(err, account.ErrCodeInvalid), errors.Is(err, account.ErrCodeExpired):
		return timed(status(0, "Invalid code submitted. Resend to receive correct activation code"), s.now()), nil
	case err != nil:
		return nil, err
	case reply != nil:
		return reply, nil
	}
	return map[string]any{"status": 1, "message": "Successfully updated user phone number", "phone": strings.TrimPrefix(pc.NewPhone, "+")}, nil
}

func (s *Server) updateUserMobileToken(ctx context.Context, c *call) (any, error) {
	tok := c.req.Str("mobile_token")
	if tok == "" {
		return status(0, "Mobile token is required to update user profile"), nil
	}
	if err := s.q.SetUserMobileToken(ctx, store.SetUserMobileTokenParams{ID: c.userID, MobileToken: tok}); err != nil {
		return nil, err
	}
	return status(1, "Token successfully updated"), nil
}

func (s *Server) changeUserPassword(ctx context.Context, c *call) (any, error) {
	errs := map[string]string{}
	old, pw, confirm := c.req.Str("old_password"), c.req.Str("password"), c.req.Str("confirm_password")
	switch {
	case old == "":
		errs["old_password"] = "The Old Password field is required."
	}
	switch {
	case pw == "":
		errs["password"] = "The Password field is required."
	case len(pw) < account.MinPasswordLen:
		errs["password"] = fmt.Sprintf("The Password field must be at least %d characters in length.", account.MinPasswordLen)
	}
	switch {
	case confirm == "":
		errs["confirm_password"] = "The Confirm Password field is required."
	case confirm != pw:
		errs["confirm_password"] = "The Confirm Password field does not match the Password field."
	}
	if len(errs) > 0 {
		return timed(map[string]any{"status": 0, "message": "Form validation failed", "validation_errors": errs}, s.now()), nil
	}
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	if u.PasswordHash == nil || bcrypt.CompareHashAndPassword([]byte(*u.PasswordHash), []byte(old)) != nil {
		return timed(status(0, "Password change failed: Wrong password entered"), s.now()), nil
	}
	hash, err := account.HashPassword(pw)
	if err != nil {
		return timed(status(0, strings.ToLower(err.Error())), s.now()), nil
	}
	// The app stays signed in; web sessions are signed out.
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := q.ResetUserPassword(ctx, store.ResetUserPasswordParams{ID: u.ID, PasswordHash: &hash}); err != nil {
			return err
		}
		return q.RevokeUserRefreshTokens(ctx, u.ID)
	})
	if err != nil {
		return nil, err
	}
	return timed(status(1, "Password successfully changed"), s.now()), nil
}

// checkUserValidationStatus: the legacy app confirmed members' identity
// against their MPesa registration (Safaricom KYC). That check is not
// carried over, so an unvalidated user is asked to retry, as legacy did
// before a check came back.
func (s *Server) checkUserValidationStatus(ctx context.Context, c *call) (any, error) {
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	if u.IsValidated {
		return timed(map[string]any{"status": 1, "message": "Verification successful", "is_user_validated": 1}, s.now()), nil
	}
	return timed(map[string]any{"status": 1, "message": "Retry", "retry_request": 1}, s.now()), nil
}

func (s *Server) validateUser(ctx context.Context, c *call) (any, error) {
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	if u.IsValidated {
		return timed(map[string]any{"status": 1, "is_user_validated": 1, "message": "User already validated"}, s.now()), nil
	}
	return timed(status(0, "Identity confirmation is not available at the moment. Try again later"), s.now()), nil
}

// getUserGroupDataInformation is the size of the offline data set, which the
// app uses to decide how many pages to fetch. This server sends the data
// in one page, so it answers one.
func (s *Server) getUserGroupDataInformation(ctx context.Context, c *call) (any, error) {
	return map[string]any{"data_length": 0, "number_of_queries": 1}, nil
}

func (s *Server) saveActivityLog(ctx context.Context, c *call) (any, error) {
	var gid *int64
	if id := c.req.Int("group_id"); id > 0 {
		gid = &id
	}
	data := c.req.Str("data")
	if data == "" {
		if b, err := jsonText(c.req["data"]); err == nil {
			data = b
		}
	}
	if err := s.q.InsertAppActivityLog(ctx, store.InsertAppActivityLogParams{UserID: &c.userID, GroupID: gid, Data: data}); err != nil {
		return nil, err
	}
	return timed(status(1, "Successfully saved activity log"), s.now()), nil
}

func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// ---- Photos ----

// uploadActions take a multipart form (a file named "avatar") instead of an
// encrypted body, as legacy _is_image_upload allowed.
var uploadActions = map[string]bool{
	"users/edit_profile_photo": true, "groups/edit_profile_photo": true, "members/edit_profile_photo": true,
}

const maxPhotoBytes = 8 << 20

// avatarsDir is where photos go: <uploads>/groups, the legacy folder.
func (s *Server) avatarsDir() (string, error) {
	if s.uploads == "" {
		return "", errors.New("mobile: uploads folder not set")
	}
	dir := filepath.Join(s.uploads, "groups")
	return dir, os.MkdirAll(dir, 0o755)
}

func photoName(prefix string, id int64, now time.Time) string {
	return fmt.Sprintf("%s-%d-%d.jpeg", books.Slug(prefix), id, now.UnixNano())
}

// savePhoto writes the image, checking it is one, and removes the old one.
func (s *Server) savePhoto(data []byte, name, old string) error {
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		return errBadImage
	}
	dir, err := s.avatarsDir()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return err
	}
	if old != "" && filepath.Base(old) == old {
		_ = os.Remove(filepath.Join(dir, old))
	}
	return nil
}

var errBadImage = errors.New("not an image")

// base64Image reads a data URI or bare base64 image.
func base64Image(v string) ([]byte, error) {
	if _, rest, ok := strings.Cut(v, ";base64,"); ok {
		v = rest
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(v))
	}
	if err != nil || len(b) > maxPhotoBytes {
		return nil, errBadImage
	}
	return b, nil
}

func (s *Server) editNewProfilePhoto(ctx context.Context, c *call) (any, error) {
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	data, err := base64Image(c.req.Str("avatar"))
	if err != nil {
		return status(0, "Error updating your profile picture"), nil
	}
	return s.setUserPhoto(ctx, u, data)
}

func (s *Server) setUserPhoto(ctx context.Context, u store.User, data []byte) (any, error) {
	name := photoName(u.FirstName+u.LastName, u.ID, s.now())
	if err := s.savePhoto(data, name, u.Avatar); errors.Is(err, errBadImage) {
		return status(0, "Invalid file format name"), nil
	} else if err != nil {
		return nil, err
	}
	if err := s.q.SetUserAvatar(ctx, store.SetUserAvatarParams{ID: u.ID, Avatar: name}); err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "successful", "avatar": name}, nil
}

// uploadedPhoto reads the "avatar" file of a multipart upload.
func uploadedPhoto(c *call) ([]byte, map[string]any) {
	if c.files == nil || len(c.files["avatar"]) == 0 {
		return nil, map[string]any{"status": 2, "error": "No files sent"}
	}
	f, err := c.files["avatar"][0].Open()
	if err != nil {
		return nil, status(0, "Error uploading profile picture")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPhotoBytes+1))
	if err != nil || len(data) > maxPhotoBytes {
		return nil, status(0, "Error uploading profile picture")
	}
	return data, nil
}

func (s *Server) editProfilePhoto(ctx context.Context, c *call) (any, error) {
	data, reply := uploadedPhoto(c)
	if reply != nil {
		return reply, nil
	}
	u, err := s.me(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.setUserPhoto(ctx, u, data)
}

// editMemberProfilePhoto sets another member's photo (officials) or one's
// own; legacy took member_id.
func (s *Server) editMemberProfilePhoto(ctx context.Context, c *call, g *member) (any, error) {
	data, reply := uploadedPhoto(c)
	if reply != nil {
		return reply, nil
	}
	mid := c.req.Int("member_id")
	if mid == 0 || mid == g.m.ID {
		return s.setUserPhoto(ctx, g.user, data)
	}
	if !g.official() {
		return timed(status(0, "You are not allowed to perform this request. For group admins only"), s.now()), nil
	}
	m, err := s.q.GetGroupMemberUser(ctx, store.GetGroupMemberUserParams{GroupID: g.group.ID, ID: mid})
	if errors.Is(err, pgx.ErrNoRows) {
		return status(0, "Could not find member details"), nil
	}
	if err != nil {
		return nil, err
	}
	u, err := s.q.GetUserByID(ctx, m.UserID)
	if err != nil {
		return nil, err
	}
	return s.setUserPhoto(ctx, u, data)
}

func (s *Server) setGroupPhoto(ctx context.Context, g *member, data []byte) (any, error) {
	name := photoName(g.group.Name, g.group.ID, s.now())
	old, _ := g.settings["avatar"].(string)
	if err := s.savePhoto(data, name, old); errors.Is(err, errBadImage) {
		return status(0, "Invalid file format name"), nil
	} else if err != nil {
		return nil, err
	}
	if err := s.setGroupFields(ctx, g, store.SetGroupFieldsParams{}, map[string]any{"avatar": name}); err != nil {
		return nil, err
	}
	return map[string]any{"status": 1, "message": "successful", "avatar": name}, nil
}

func (s *Server) editGroupProfilePhoto(ctx context.Context, c *call, g *member) (any, error) {
	data, reply := uploadedPhoto(c)
	if reply != nil {
		return reply, nil
	}
	return s.setGroupPhoto(ctx, g, data)
}

func (s *Server) editNewGroupProfilePhoto(ctx context.Context, c *call, g *member) (any, error) {
	data, err := base64Image(c.req.Str("avatar"))
	if err != nil {
		return status(0, "Error updating group profile picture"), nil
	}
	return s.setGroupPhoto(ctx, g, data)
}
