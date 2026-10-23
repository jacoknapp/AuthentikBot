package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

var errRecoveryProtected = errors.New("Target is administrative or eligibility metadata is incomplete; recovery denied")
var recoveryPermissionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func validRecoveryUUID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func systemRecoveryUser(u recoveryUser) bool {
	name := strings.ToLower(u.Username)
	if name == "anonymoususer" || name == "anonymous" || name == "system" {
		return true
	}
	if u.Path == nil {
		return true
	}
	path := strings.ToLower(strings.Trim(*u.Path, "/"))
	return path == "goauthentik.io" || strings.HasPrefix(path, "goauthentik.io/")
}

type recoveryRole struct {
	PK string `json:"pk"`
}
type recoveryPermission struct {
	ID     int64   `json:"id"`
	App    string  `json:"app_label"`
	Model  string  `json:"model"`
	Code   string  `json:"codename"`
	Object *string `json:"object_pk,omitempty"`
}

func (p recoveryPermission) ordinary(object bool) bool {
	if p.ID <= 0 || !recoveryPermissionName.MatchString(p.App) || !recoveryPermissionName.MatchString(p.Model) || !recoveryPermissionName.MatchString(p.Code) {
		return false
	}
	if object {
		if p.Object == nil || strings.TrimSpace(*p.Object) == "" {
			return false
		}
	} else if p.Object != nil {
		return false
	}
	switch p.App + "." + p.Code {
	case "authentik_core.view_application":
		return p.Model == "application"
	case "authentik_providers_ldap.search_full_directory":
		return p.Model == "ldapprovider"
	default:
		return false
	}
}
func (a *recoveryAPI) effectiveRoles(pk int64) ([]string, error) {
	roles, err := recoveryList[recoveryRole](a, "/api/v3/rbac/roles/", url.Values{"users": {strconv.FormatInt(pk, 10)}, "inherited": {"true"}})
	if err != nil {
		return nil, err
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, r := range roles {
		if !validRecoveryUUID(r.PK) || seen[r.PK] {
			return nil, errRecoveryProtected
		}
		seen[r.PK] = true
		ids = append(ids, r.PK)
	}
	sort.Strings(ids)
	return ids, nil
}

// Role names/group names never determine privilege. Canonical snapshots include
// every effective role and returned permission's app/model/codename/object scope.
func (a *recoveryAPI) permissionSnapshot(pk int64, fullVisibility bool) (string, error) {
	ids, err := a.effectiveRoles(pk)
	if err != nil {
		return "", err
	}
	entries := []string{}
	for _, id := range ids {
		entries = append(entries, "role:"+id)
		var global, objects []recoveryPermission
		for _, object := range []bool{false, true} {
			path := "/api/v3/rbac/permissions/"
			q := url.Values{"role": {id}}
			if object {
				path = "/api/v3/rbac/permissions/roles/"
				q = url.Values{"uuid": {id}, "page_size": {"10"}}
			}
			perms, e := recoveryList[recoveryPermission](a, path, q)
			if e != nil {
				return "", e
			}
			if object {
				objects = perms
			} else {
				global = perms
			}
			seen := map[int64]bool{}
			for _, p := range perms {
				if !p.ordinary(object) || seen[p.ID] {
					return "", errRecoveryProtected
				}
				seen[p.ID] = true
				b, _ := json.Marshal(p)
				scope := "global:"
				if object {
					scope = "object:"
				}
				entries = append(entries, id+":"+scope+string(b))
			}
		}
		assignments, e := a.completeRoleAssignments(id, global, objects, fullVisibility)
		if e != nil {
			return "", e
		}
		entries = append(entries, assignments...)
	}
	after, e := a.effectiveRoles(pk)
	if e != nil {
		return "", e
	}
	if strings.Join(ids, ",") != strings.Join(after, ",") {
		return "", errRecoveryProtected
	}
	sort.Strings(entries)
	b, _ := json.Marshal(entries)
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}
func (a *recoveryAPI) ordinaryPermissions(pk int64, fullVisibility bool) (string, error) {
	first, err := a.permissionSnapshot(pk, fullVisibility)
	if err != nil {
		return "", err
	}
	second, err := a.permissionSnapshot(pk, fullVisibility)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errRecoveryProtected
	}
	return first, nil
}

// Native identification uses username OR email case-insensitively and picks the
// first result. Scan all visible users, including protected/inactive accounts:
// excluding them from this check could route the native email to the wrong user.
func (a *recoveryAPI) uniqueNativeIdentifier(target recoveryUser) error {
	if target.Email == nil || strings.TrimSpace(*target.Email) == "" {
		return errors.New("Target has no email address; no native request submitted")
	}
	users, err := recoveryList[recoveryUser](a, "/api/v3/core/users/", url.Values{"include_groups": {"false"}, "include_roles": {"false"}, "ordering": {"pk"}})
	if err != nil {
		return err
	}
	matches := 0
	seen := map[int64]bool{}
	identifier := recoveryIdentifierFold(target.Username)
	fold := func(s string) bool {
		// Include simple upper/lower comparisons as a conservative superset for
		// database locale variants as well as full Unicode case-fold expansions.
		return recoveryIdentifierFold(s) == identifier || strings.EqualFold(s, target.Username) || strings.ToLower(s) == strings.ToLower(target.Username) || strings.ToUpper(s) == strings.ToUpper(target.Username)
	}
	for _, u := range users {
		if u.PK <= 0 || !validRecoveryUUID(u.UUID) || u.Username == "" || u.Email == nil || seen[u.PK] {
			return errRecoveryProtected
		}
		seen[u.PK] = true
		if fold(u.Username) || fold(*u.Email) {
			if !target.same(u) {
				return errors.New("Native recovery identifier is ambiguous or changed; no request submitted")
			}
			matches++
		}
	}
	if matches != 1 {
		return errors.New("Native recovery identifier is not uniquely visible; no request submitted")
	}
	return nil
}

// The permission catalog filters out non-authentik app labels. The assigned-by-
// roles serializer returns ALL nested global/object assignments for each matched
// role, even when its model filter selected the role via one ordinary grant.
// No visible anchor means completeness cannot be proven: fail closed rather than
// infer that an apparently empty role has no hidden third-party grants.
type recoveryRoleAssignments struct {
	Role    string               `json:"role_pk"`
	Global  []recoveryPermission `json:"model_permissions"`
	Objects []recoveryPermission `json:"object_permissions"`
}

func permissionMeaning(p recoveryPermission) string {
	b, _ := json.Marshal(struct {
		App, Model, Code string
		Object           *string
	}{p.App, p.Model, p.Code, p.Object})
	return string(b)
}
func samePermissionMeanings(a, b []recoveryPermission) bool {
	if len(a) != len(b) {
		return false
	}
	keys := map[string]bool{}
	for _, p := range a {
		key := permissionMeaning(p)
		if keys[key] {
			return false
		}
		keys[key] = true
	}
	for _, p := range b {
		key := permissionMeaning(p)
		if !keys[key] {
			return false
		}
		delete(keys, key)
	}
	return len(keys) == 0
}
func (a *recoveryAPI) completeRoleAssignments(role string, global, objects []recoveryPermission, fullVisibility bool) ([]string, error) {
	q := url.Values{}
	if len(global) > 0 {
		q.Set("model", global[0].App+"."+global[0].Model)
	} else if len(objects) > 0 {
		q.Set("model", objects[0].App+"."+objects[0].Model)
		q.Set("object_pk", *objects[0].Object)
	} else {
		// Only a freshly verified superuser can prove that used_by omitted no
		// guardian assignments due to visibility filtering. Separate credentials
		// retain the conservative denial until equivalent visibility is proven.
		if !fullVisibility {
			return nil, errRecoveryProtected
		}
		var refs []struct {
			App   string `json:"app"`
			Model string `json:"model_name"`
			PK    string `json:"pk"`
		}
		if err := a.request("GET", "/api/v3/rbac/roles/"+role+"/used_by/", nil, nil, &refs); err != nil {
			return nil, err
		}
		if refs == nil || len(refs) > 10000 {
			return nil, errRecoveryProtected
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			if ref.App == "" || ref.Model == "" || ref.PK == "" || seen[ref.App+"."+ref.Model+":"+ref.PK] {
				return nil, errRecoveryProtected
			}
			seen[ref.App+"."+ref.Model+":"+ref.PK] = true
			if ref.App == "guardian" {
				return nil, errRecoveryProtected
			}
		}
		return []string{role + ":proven-empty"}, nil
	}
	results, err := recoveryList[recoveryRoleAssignments](a, "/api/v3/rbac/permissions/assigned_by_roles/", q)
	if err != nil {
		return nil, err
	}
	found := false
	entries := []string{}
	seenRoles := map[string]bool{}
	for _, r := range results {
		if !validRecoveryUUID(r.Role) || seenRoles[r.Role] {
			return nil, errRecoveryProtected
		}
		seenRoles[r.Role] = true
		if r.Role != role {
			continue
		}
		if found || r.Global == nil || r.Objects == nil {
			return nil, errRecoveryProtected
		}
		found = true
		for _, scope := range []struct {
			perms  []recoveryPermission
			object bool
		}{{r.Global, false}, {r.Objects, true}} {
			seen := map[int64]bool{}
			for _, p := range scope.perms {
				if !p.ordinary(scope.object) || seen[p.ID] {
					return nil, errRecoveryProtected
				}
				seen[p.ID] = true
				b, _ := json.Marshal(p)
				entries = append(entries, role+":assignment:"+string(b))
			}
		}
		if !samePermissionMeanings(global, r.Global) || !samePermissionMeanings(objects, r.Objects) {
			return nil, errRecoveryProtected
		}
	}
	if !found {
		return nil, errRecoveryProtected
	}
	return entries, nil
}
