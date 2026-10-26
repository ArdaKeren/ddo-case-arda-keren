package auth

// Role values must match the CHECK constraint on "qa-ev-users".role.
type Role string

const (
	RoleAdmin   Role = "admin"
	RoleStaff   Role = "staff"
	RoleDisplay Role = "display"
	RoleUser    Role = "user"
)

type Permission string

const (
	PermProfileRead        Permission = "profile:read"
	PermUsersRead          Permission = "users:read"
	PermUsersWrite         Permission = "users:write"
	PermAuditRead          Permission = "audit:read"
	PermTicketsCreate      Permission = "tickets:create"
	PermAppointmentsCreate Permission = "appointments:create"
	PermQueueRead          Permission = "queue:read"
	PermQueueCall          Permission = "queue:call"
	PermQueueManage        Permission = "queue:manage"
)

var rolePermissions = map[Role]map[Permission]struct{}{
	RoleUser:    set(PermProfileRead, PermTicketsCreate, PermAppointmentsCreate),
	RoleDisplay: set(PermProfileRead, PermQueueRead),
	RoleStaff: set(
		PermProfileRead,
		PermTicketsCreate,
		PermAppointmentsCreate,
		PermQueueRead,
		PermQueueCall,
	),
	RoleAdmin: set(
		PermProfileRead,
		PermUsersRead,
		PermUsersWrite,
		PermAuditRead,
		PermTicketsCreate,
		PermAppointmentsCreate,
		PermQueueRead,
		PermQueueCall,
		PermQueueManage,
	),
}

func set(perms ...Permission) map[Permission]struct{} {
	m := make(map[Permission]struct{}, len(perms))
	for _, p := range perms {
		m[p] = struct{}{}
	}
	return m
}

func (r Role) Valid() bool {
	_, ok := rolePermissions[r]
	return ok
}

func (r Role) Can(p Permission) bool {
	_, ok := rolePermissions[r][p]
	return ok
}

func (r Role) Permissions() []Permission {
	out := make([]Permission, 0, len(rolePermissions[r]))
	for p := range rolePermissions[r] {
		out = append(out, p)
	}
	return out
}
