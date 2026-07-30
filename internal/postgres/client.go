package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	databasev1 "postgres-operator/api/v1"
)

type Client struct {
	db *sql.DB
}

func NewClientForInstance(
    ctx context.Context,
    k8sClient client.Client,
    cluster *databasev1.PostgresCluster,
    instanceName string,
) (*Client, error) {
    secret := &corev1.Secret{}
    err := k8sClient.Get(ctx, types.NamespacedName{
        Name:      cluster.Name + "-credentials",
        Namespace: cluster.Namespace,
    }, secret)
    if err != nil {
        return nil, fmt.Errorf("failed to get credentials secret: %w", err)
    }

    password, exists := secret.Data["postgres-password"]
    if !exists {
        return nil, fmt.Errorf("postgres-password not found in secret")
    }

    host := fmt.Sprintf("%s-%s.%s.svc.cluster.local", cluster.Name, instanceName, cluster.Namespace)
    connStr := fmt.Sprintf("host=%s port=5432 user=postgres password=%s dbname=%s sslmode=require connect_timeout=10",
        host, string(password), cluster.Spec.Database.Name)

    db, err := sql.Open("postgres", connStr)
    if err != nil {
        return nil, fmt.Errorf("failed to open database connection to instance %s: %w", instanceName, err)
    }

    db.SetMaxOpenConns(5)
    db.SetMaxIdleConns(2)
    db.SetConnMaxLifetime(30 * time.Minute)

    ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
    defer cancel()

    if err := db.PingContext(ctx); err != nil {
        db.Close()
        return nil, fmt.Errorf("failed to ping instance %s: %w", instanceName, err)
    }

    return &Client{db: db}, nil
}

func (c *Client) SetConnectionLimit(ctx context.Context, username string, limit int32) error {
    if username == "" {
        return fmt.Errorf("username cannot be empty")
    }
    
    if limit < -1 {
        return fmt.Errorf("invalid connection limit: %d (must be -1 for no limit or >= 0)", limit)
    }

    query := fmt.Sprintf("ALTER USER %s WITH CONNECTION LIMIT %d", pq.QuoteIdentifier(username), limit)
    _, err := c.db.ExecContext(ctx, query)
    if err != nil {
        return fmt.Errorf("failed to set connection limit for user %s: %w", username, err)
    }

    return nil
}

func (c *Client) Close() error {
	return c.db.Close()
}

func (c *Client) CreateOrUpdateUser(ctx context.Context, username, password string, privileges []string) error {
	if username == "" || password == "" {
		return fmt.Errorf("username and password cannot be empty")
	}

	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_user WHERE usename = $1)", username).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	if exists {
		query := fmt.Sprintf("ALTER USER %s WITH PASSWORD $1", pq.QuoteIdentifier(username))
		_, err = c.db.ExecContext(ctx, query, password)
		if err != nil {
			return fmt.Errorf("failed to update user password: %w", err)
		}
	} else {
		query := fmt.Sprintf("CREATE USER %s WITH PASSWORD $1", pq.QuoteIdentifier(username))
		_, err = c.db.ExecContext(ctx, query, password)
		if err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}
	}

	for _, privilege := range privileges {
		query := fmt.Sprintf("GRANT %s TO %s", privilege, pq.QuoteIdentifier(username))
		_, err = c.db.ExecContext(ctx, query)
		if err != nil {
			return fmt.Errorf("failed to grant privilege %s: %w", privilege, err)
		}
	}

	return nil
}

func (c *Client) DropUser(ctx context.Context, username string) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}

	systemUsers := []string{"postgres", "template0", "template1", "pg_monitor", "pg_read_all_settings", "pg_read_all_stats", "pg_stat_scan_tables", "pg_read_server_files", "pg_write_server_files", "pg_execute_server_program"}
	for _, sysUser := range systemUsers {
		if username == sysUser {
			return fmt.Errorf("cannot drop system user: %s", username)
		}
	}

	queries := []string{
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON DATABASE %s FROM %s", pq.QuoteIdentifier(c.getCurrentDatabase(ctx)), pq.QuoteIdentifier(username)),
	}

	for _, query := range queries {
		_, err := c.db.ExecContext(ctx, query)
		if err != nil {
			fmt.Printf("Warning: failed to revoke privileges with query '%s': %v\n", query, err)
		}
	}

	query := fmt.Sprintf("DROP USER IF EXISTS %s", pq.QuoteIdentifier(username))
	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to drop user: %w", err)
	}

	return nil
}

func (c *Client) GrantDatabaseAccess(ctx context.Context, username, database string) error {
	if username == "" || database == "" {
		return fmt.Errorf("username and database cannot be empty")
	}

	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_user WHERE usename = $1)", username).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	if !exists {
		return fmt.Errorf("user %s does not exist", username)
	}

	query := fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", pq.QuoteIdentifier(database), pq.QuoteIdentifier(username))
	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant connect privilege: %w", err)
	}

	query = fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", pq.QuoteIdentifier(username))
	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant schema usage: %w", err)
	}

	query = fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", pq.QuoteIdentifier(username))
	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant table privileges: %w", err)
	}

	query = fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", pq.QuoteIdentifier(username))
	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant sequence privileges: %w", err)
	}

	return nil
}

func (c *Client) RevokeDatabaseAccess(ctx context.Context, username, database string) error {
	if username == "" || database == "" {
		return fmt.Errorf("username and database cannot be empty")
	}

	queries := []string{
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE USAGE ON SCHEMA public FROM %s", pq.QuoteIdentifier(username)),
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM %s", pq.QuoteIdentifier(database), pq.QuoteIdentifier(username)),
	}

	for _, query := range queries {
		_, err := c.db.ExecContext(ctx, query)
		if err != nil {
			return fmt.Errorf("failed to execute revoke query '%s': %w", query, err)
		}
	}

	return nil
}

func (c *Client) GetDatabaseStats(ctx context.Context) (*DatabaseStats, error) {
	stats := &DatabaseStats{}

	err := c.db.QueryRowContext(ctx, "SELECT pg_database_size(current_database())").Scan(&stats.Size)
	if err != nil {
		return nil, fmt.Errorf("failed to get database size: %w", err)
	}

	err = c.db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()").Scan(&stats.Connections)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection count: %w", err)
	}

	err = c.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'").Scan(&stats.Tables)
	if err != nil {
		return nil, fmt.Errorf("failed to get table count: %w", err)
	}

	return stats, nil
}

func (c *Client) getCurrentDatabase(ctx context.Context) string {

	
	var dbName string
	err := c.db.QueryRowContext(ctx, "SELECT current_database()").Scan(&dbName)
	if err != nil {
		return "postgres" // fallback to default
	}
	return dbName
}

type DatabaseStats struct {
	Size        int64 `json:"size"`
	Connections int   `json:"connections"`
	Tables      int   `json:"tables"`
}