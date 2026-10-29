package schema_test

import (
	"reflect"
	"testing"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/schema"
)

func usersTable() schema.Table {
	return schema.Table{
		Name:        "users",
		Owner:       "core",
		Columns:     []schema.Column{{Name: "id", Type: schema.ColTypeText, PrimaryKey: true}, {Name: "email", Type: schema.ColTypeText}},
		Indexes:     []schema.Index{{Name: "idx_users_email", Columns: []string{"email"}, Unique: true}},
		ForeignKeys: []schema.ForeignKey{{Name: "fk_users_team", Columns: []string{"team_id"}, RefTable: "teams", RefColumns: []string{"id"}}},
	}
}

// A table reads as its declaration with every contribution after it, in the
// order they came, whether a contribution came before the table or after.
func TestRegistryMergesContributions(t *testing.T) {
	r := schema.NewRegistry()
	phone := schema.Column{Name: "phone_number", Type: schema.ColTypeText, Nullable: true}
	if err := r.ExtendColumn(schema.ColumnContribution{Table: "users", Column: phone, Owner: "phonenumber"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Declare(&models.User{}, usersTable()); err != nil {
		t.Fatal(err)
	}
	if err := r.Declare(&models.Session{}, schema.Table{Name: "sessions", Owner: "core"}); err != nil {
		t.Fatal(err)
	}
	byPhone := schema.Index{Name: "idx_users_phone", Columns: []string{"phone_number"}, Unique: true}
	if err := r.ExtendIndex(schema.IndexContribution{Table: "users", Index: byPhone, Owner: "phonenumber"}); err != nil {
		t.Fatal(err)
	}
	toOrg := schema.ForeignKey{Name: "fk_users_org", Columns: []string{"org_id"}, RefTable: "orgs", RefColumns: []string{"id"}}
	if err := r.ExtendForeignKey(schema.ForeignKeyContribution{Table: "users", ForeignKey: toOrg, Owner: "orgs"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}

	users, ok := r.Lookup("users")
	if !ok {
		t.Fatal("users is not found")
	}
	want := usersTable()
	want.Columns = append(want.Columns, phone)
	want.Indexes = append(want.Indexes, byPhone)
	want.ForeignKeys = append(want.ForeignKeys, toOrg)
	if !reflect.DeepEqual(users, want) {
		t.Errorf("users:\n got %+v\nwant %+v", users, want)
	}
	if all := r.All(); len(all) != 2 || all[0].Name != "sessions" || !reflect.DeepEqual(all[1], want) {
		t.Errorf("All: got %+v, want sessions, then the merged users", all)
	}
	if m, ok := r.LookupModel("users"); !ok || m.SchemaName() != "users" {
		t.Errorf("LookupModel(users): got %v, %v", m, ok)
	}
	if _, ok := r.Lookup("teams"); ok {
		t.Error("a table nobody declared is found")
	}
	if _, ok := r.LookupModel("teams"); ok {
		t.Error("a model nobody declared is found")
	}
}

// A declaration or a contribution that would make a table ambiguous is a
// configuration error: a second table of one name, a contribution that
// redefines a name of its table or of another contribution, a contribution
// to a table nobody declares, and any change after Freeze.
func TestRegistryRefusesConflicts(t *testing.T) {
	column := func(name, owner string) schema.ColumnContribution {
		return schema.ColumnContribution{Table: "users", Column: schema.Column{Name: name, Type: schema.ColTypeText}, Owner: owner}
	}
	index := func(name, owner string) schema.IndexContribution {
		return schema.IndexContribution{Table: "users", Index: schema.Index{Name: name, Columns: []string{"email"}}, Owner: owner}
	}
	foreignKey := func(name, owner string) schema.ForeignKeyContribution {
		return schema.ForeignKeyContribution{Table: "users", ForeignKey: schema.ForeignKey{Name: name, Columns: []string{"id"}, RefTable: "teams", RefColumns: []string{"id"}}, Owner: owner}
	}
	declared := func(t *testing.T) *schema.DefaultRegistry {
		t.Helper()
		r := schema.NewRegistry()
		if err := r.Declare(&models.User{}, usersTable()); err != nil {
			t.Fatal(err)
		}
		return r
	}
	frozen := func(t *testing.T) *schema.DefaultRegistry {
		t.Helper()
		r := declared(t)
		if err := r.Freeze(); err != nil {
			t.Fatal(err)
		}
		return r
	}

	for name, try := range map[string]func(t *testing.T) error{
		"a table under another name": func(t *testing.T) error {
			return schema.NewRegistry().Declare(&models.User{}, schema.Table{Name: "accounts"})
		},
		"a table declared twice": func(t *testing.T) error { return declared(t).Declare(&models.User{}, usersTable()) },
		"a column of the table":  func(t *testing.T) error { return declared(t).ExtendColumn(column("email", "other")) },
		"an index of the table": func(t *testing.T) error {
			return declared(t).ExtendIndex(index("idx_users_email", "other"))
		},
		"a foreign key of the table": func(t *testing.T) error {
			return declared(t).ExtendForeignKey(foreignKey("fk_users_team", "other"))
		},
		"a column contributed twice": func(t *testing.T) error {
			r := declared(t)
			if err := r.ExtendColumn(column("phone", "a")); err != nil {
				t.Fatal(err)
			}
			return r.ExtendColumn(column("phone", "b"))
		},
		"an index contributed twice": func(t *testing.T) error {
			r := declared(t)
			if err := r.ExtendIndex(index("idx_phone", "a")); err != nil {
				t.Fatal(err)
			}
			return r.ExtendIndex(index("idx_phone", "b"))
		},
		"a foreign key contributed twice": func(t *testing.T) error {
			r := declared(t)
			if err := r.ExtendForeignKey(foreignKey("fk_phone", "a")); err != nil {
				t.Fatal(err)
			}
			return r.ExtendForeignKey(foreignKey("fk_phone", "b"))
		},
		"a column of the table, contributed before it": func(t *testing.T) error {
			r := schema.NewRegistry()
			if err := r.ExtendColumn(column("email", "other")); err != nil {
				t.Fatal(err)
			}
			return r.Declare(&models.User{}, usersTable())
		},
		"an index of the table, contributed before it": func(t *testing.T) error {
			r := schema.NewRegistry()
			if err := r.ExtendIndex(index("idx_users_email", "other")); err != nil {
				t.Fatal(err)
			}
			return r.Declare(&models.User{}, usersTable())
		},
		"a foreign key of the table, contributed before it": func(t *testing.T) error {
			r := schema.NewRegistry()
			if err := r.ExtendForeignKey(foreignKey("fk_users_team", "other")); err != nil {
				t.Fatal(err)
			}
			return r.Declare(&models.User{}, usersTable())
		},
		"a column of a table nobody declares": func(t *testing.T) error {
			r := schema.NewRegistry()
			if err := r.ExtendColumn(column("phone", "a")); err != nil {
				t.Fatal(err)
			}
			return r.Freeze()
		},
		"an index of a table nobody declares": func(t *testing.T) error {
			r := schema.NewRegistry()
			if err := r.ExtendIndex(index("idx_phone", "a")); err != nil {
				t.Fatal(err)
			}
			return r.Freeze()
		},
		"a foreign key of a table nobody declares": func(t *testing.T) error {
			r := schema.NewRegistry()
			if err := r.ExtendForeignKey(foreignKey("fk_phone", "a")); err != nil {
				t.Fatal(err)
			}
			return r.Freeze()
		},
		"a table after Freeze": func(t *testing.T) error {
			return frozen(t).Declare(&models.Session{}, schema.Table{Name: "sessions"})
		},
		"a column after Freeze":      func(t *testing.T) error { return frozen(t).ExtendColumn(column("phone", "a")) },
		"an index after Freeze":      func(t *testing.T) error { return frozen(t).ExtendIndex(index("idx_phone", "a")) },
		"a foreign key after Freeze": func(t *testing.T) error { return frozen(t).ExtendForeignKey(foreignKey("fk_phone", "a")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := try(t); !errs.IsCategory(err, errs.CategoryConfiguration) {
				t.Errorf("got %v, want a configuration error", err)
			}
		})
	}
}
