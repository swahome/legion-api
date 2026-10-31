package mobile

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/auth"
	"github.com/swahome/legion-api/internal/store"
)

// Income, expense and fine categories from the app: create, edit, hide,
// unhide and delete, with each module's legacy wording. The legacy app let
// any member do these; here, as on the web, only officials can.

type categoryWords struct {
	created, updated         func(name string) map[string]any
	notFound                 func(verb string) string
	alreadyHidden, alreadyOn string
	hidden, shown            map[string]any
	updateFailed             string
	used, deleted            string
}

var categoryText = map[string]categoryWords{
	"income": {
		created: func(n string) map[string]any {
			return map[string]any{"status": 1, "success": n + "  successfully added"}
		},
		updated: func(n string) map[string]any {
			return map[string]any{"status": 1, "success": n + "  successfully updated"}
		},
		notFound:      func(string) string { return "Could not find group income category" },
		alreadyHidden: "Income category already hidden", alreadyOn: "Income category already active",
		hidden: map[string]any{"status": 1, "message": "success"}, shown: map[string]any{"status": 1, "message": "success"},
		updateFailed: "Could not complete updating income category",
		used:         "Income category has existing transactions and can not be deleted",
		deleted:      "Income category successfully deleted",
	},
	"expense": {
		created: func(n string) map[string]any { return map[string]any{"status": 1, "message": n + " created"} },
		updated: func(string) map[string]any {
			return map[string]any{"status": 1, "message": "Expense category successfully updated"}
		},
		notFound:      func(string) string { return "Expense category not found" },
		alreadyHidden: "Expense category already hidden", alreadyOn: "Expense category already visible",
		hidden:       map[string]any{"status": 1, "message": "Expense category successfully hidden"},
		shown:        map[string]any{"status": 1, "message": "Expense category successfully unhidden"},
		updateFailed: "Unable to update expense category",
		used:         "Expense category has existing transactions and can not be deleted",
		deleted:      "Expense category successfully deleted",
	},
	"fine": {
		created: func(n string) map[string]any {
			return map[string]any{"status": 1, "success": n + " as a fine category was successfully created"}
		},
		updated: func(string) map[string]any {
			return map[string]any{"status": 1, "success": "Fine category successfully updated"}
		},
		notFound:      func(verb string) string { return "Could not find fine category to " + verb },
		alreadyHidden: "Sorry, the Fine Category is already hidden", alreadyOn: "Sorry, the Fine Category is already active",
		hidden: map[string]any{"status": 1, "message": "Success"}, shown: map[string]any{"status": 1, "message": "Success"},
		updateFailed: "Error updating fine category",
		used:         "Fine category has existing transactions and can not be deleted",
		deleted:      "Fine category successfully deleted",
	},
}

// categoryRow is what the writes need to know about an existing category.
type categoryRow struct {
	id       int64
	name     string
	isHidden bool
}

func (s *Server) findCategory(ctx context.Context, kind string, groupID, id int64) (*categoryRow, error) {
	var rows []categoryRow
	switch kind {
	case "income":
		list, err := s.q.ListIncomeCategories(ctx, store.ListIncomeCategoriesParams{GroupID: groupID, IncludeInactive: true})
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			rows = append(rows, categoryRow{r.ID, r.Name, r.IsHidden})
		}
	case "expense":
		list, err := s.q.ListExpenseCategories(ctx, store.ListExpenseCategoriesParams{GroupID: groupID, IncludeInactive: true})
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			rows = append(rows, categoryRow{r.ID, r.Name, r.IsHidden})
		}
	case "fine":
		list, err := s.q.ListFineCategories(ctx, store.ListFineCategoriesParams{GroupID: groupID, IncludeInactive: true})
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			rows = append(rows, categoryRow{r.ID, r.Name, r.IsHidden})
		}
	}
	for _, r := range rows {
		if r.id == id {
			return &r, nil
		}
	}
	return nil, nil
}

// categoryForm checks the name (required, unique in the group) and, for
// fine categories, the amount.
func (s *Server) categoryForm(ctx context.Context, c *call, g *member, kind string, exceptID int64) (map[string]any, error) {
	name := c.req.Str("name")
	if name == "" {
		m := s.formFailed(map[string]string{"name": "The Name field is required."})
		m["error"] = "Form validation failed"
		return m, nil
	}
	taken, err := s.q.CategoryNameTaken(ctx, store.CategoryNameTakenParams{Kind: kind, GroupID: g.group.ID, Name: name, ExceptID: exceptID})
	if err != nil {
		return nil, err
	}
	if taken {
		m := s.formFailed(map[string]string{"slug": "A category called " + name + " already exists"})
		m["error"] = "Form validation failed"
		return m, nil
	}
	if kind == "fine" && c.req.Str("amount") != "" && appAmount(c.req.Str("amount")) == "" {
		return s.formFailed(map[string]string{"amount": "The Amount field must be a valid amount."}), nil
	}
	return nil, nil
}

func (s *Server) createCategory(kind string) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		if reply, err := s.categoryForm(ctx, c, g, kind, 0); reply != nil || err != nil {
			return reply, err
		}
		name, desc := c.req.Str("name"), c.req.Str("description")
		var err error
		switch kind {
		case "income":
			_, err = s.q.CreateIncomeCategory(ctx, store.CreateIncomeCategoryParams{GroupID: g.group.ID, Name: name, Description: desc})
		case "expense":
			_, err = s.q.CreateExpenseCategory(ctx, store.CreateExpenseCategoryParams{GroupID: g.group.ID, Name: name, Description: desc})
		case "fine":
			amt := appAmount(c.req.Str("amount"))
			if amt == "" {
				amt = "0"
			}
			p := store.CreateFineCategoryParams{GroupID: g.group.ID, Name: name, Description: desc}
			_ = p.Amount.Scan(amt)
			_, err = s.q.CreateFineCategory(ctx, p)
		}
		if err != nil {
			return nil, err
		}
		return timed(categoryText[kind].created(name), s.now()), nil
	})
}

func (s *Server) editCategory(kind string) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		words := categoryText[kind]
		cat, err := s.findCategory(ctx, kind, g.group.ID, c.req.Int("id"))
		if err != nil {
			return nil, err
		}
		if cat == nil {
			return timed(status(0, words.notFound("edit")), s.now()), nil
		}
		if reply, err := s.categoryForm(ctx, c, g, kind, cat.id); reply != nil || err != nil {
			return reply, err
		}
		name, desc := c.req.Str("name"), c.req.Str("description")
		switch kind {
		case "income":
			_, err = s.q.UpdateIncomeCategory(ctx, store.UpdateIncomeCategoryParams{GroupID: g.group.ID, ID: cat.id, Name: &name, Description: &desc})
		case "expense":
			_, err = s.q.UpdateExpenseCategory(ctx, store.UpdateExpenseCategoryParams{GroupID: g.group.ID, ID: cat.id, Name: &name, Description: &desc})
		case "fine":
			p := store.UpdateFineCategoryParams{GroupID: g.group.ID, ID: cat.id, Name: &name, Description: &desc}
			if amt := appAmount(c.req.Str("amount")); amt != "" {
				_ = p.Amount.Scan(amt)
			}
			_, err = s.q.UpdateFineCategory(ctx, p)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return timed(status(0, words.updateFailed), s.now()), nil
		}
		if err != nil {
			return nil, err
		}
		return timed(words.updated(name), s.now()), nil
	})
}

// hideCategory is hide (hidden true) and unhide (false).
func (s *Server) hideCategory(kind string, hidden bool) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		words := categoryText[kind]
		verb := "hide"
		if !hidden {
			verb = "activate"
		}
		cat, err := s.findCategory(ctx, kind, g.group.ID, c.req.Int("id"))
		if err != nil {
			return nil, err
		}
		if cat == nil {
			return timed(status(0, words.notFound(verb)), s.now()), nil
		}
		if cat.isHidden == hidden {
			if hidden {
				return timed(status(0, words.alreadyHidden), s.now()), nil
			}
			return timed(status(0, words.alreadyOn), s.now()), nil
		}
		switch kind {
		case "income":
			_, err = s.q.UpdateIncomeCategory(ctx, store.UpdateIncomeCategoryParams{GroupID: g.group.ID, ID: cat.id, IsHidden: &hidden})
		case "expense":
			_, err = s.q.UpdateExpenseCategory(ctx, store.UpdateExpenseCategoryParams{GroupID: g.group.ID, ID: cat.id, IsHidden: &hidden})
		case "fine":
			_, err = s.q.UpdateFineCategory(ctx, store.UpdateFineCategoryParams{GroupID: g.group.ID, ID: cat.id, IsHidden: &hidden})
		}
		if err != nil {
			return nil, err
		}
		reply := words.shown
		if hidden {
			reply = words.hidden
		}
		return timed(copyMap(reply), s.now()), nil
	})
}

// deleteCategory removes a category nothing has used yet.
func (s *Server) deleteCategory(kind string) groupFn {
	return s.officialsOnly(func(ctx context.Context, c *call, g *member) (any, error) {
		words := categoryText[kind]
		cat, err := s.findCategory(ctx, kind, g.group.ID, c.req.Int("id"))
		if err != nil {
			return nil, err
		}
		if cat == nil {
			return timed(status(0, words.notFound("delete")), s.now()), nil
		}
		used, err := s.q.CategoryUsed(ctx, store.CategoryUsedParams{Kind: kind, ID: cat.id})
		if err != nil {
			return nil, err
		}
		if used {
			return timed(status(0, words.used), s.now()), nil
		}
		if _, err := s.q.DeleteCategory(ctx, store.DeleteCategoryParams{Kind: kind, GroupID: g.group.ID, ID: cat.id}); err != nil {
			return nil, err
		}
		return timed(status(1, words.deleted), s.now()), nil
	})
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// createDepositor is depositors/create: someone outside the group who pays
// income in.
func (s *Server) createDepositor(ctx context.Context, c *call, g *member) (any, error) {
	name := c.req.Str("name")
	fail := func(field, msg string) map[string]any {
		m := s.formFailed(map[string]string{field: msg})
		m["error"] = "Form validation failed"
		return m
	}
	if name == "" {
		return fail("name", "The Name field is required."), nil
	}
	existing, err := s.q.ListDepositors(ctx, store.ListDepositorsParams{GroupID: g.group.ID, IncludeInactive: true})
	if err != nil {
		return nil, err
	}
	for _, d := range existing {
		if strings.EqualFold(d.Name, name) {
			return fail("name", "A depositor called "+name+" already exists"), nil
		}
	}
	p := store.CreateDepositorParams{GroupID: g.group.ID, Name: name, Description: c.req.Str("description")}
	if email := strings.ToLower(c.req.Str("email")); email != "" {
		if !strings.Contains(email, "@") || strings.ContainsAny(email, " ,;") {
			return fail("email", "The Email field must contain a valid email address."), nil
		}
		p.Email = &email
	}
	if phone := c.req.Str("phone"); phone != "" {
		n, err := auth.NormalizePhone(phone, s.auth.DefaultCallingCode())
		if err != nil {
			return fail("phone", "The Phone field must contain a valid phone number."), nil
		}
		p.Phone = &n
	}
	if _, err := s.q.CreateDepositor(ctx, p); err != nil {
		return nil, err
	}
	return timed(map[string]any{"status": 1, "success": "Depositor successfully created"}, s.now()), nil
}
