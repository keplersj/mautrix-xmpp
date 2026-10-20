package connector

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// aliasDB maps archive-assigned stanza IDs (XEP-0359) to bridged message IDs.
// Group chat references (reactions, replies, retractions, markers) point at the
// room's stanza-id, while bridged message IDs use the sender-assigned ID.
type aliasDB struct {
	*dbutil.Database
}

var aliasUpgradeTable = dbutil.BuildUpgradeTable().
	WithRaw(0, 1, 0, "Create message alias table", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
		_, err := db.Exec(ctx, `
			CREATE TABLE xmpp_message_alias (
				login_id   TEXT NOT NULL,
				portal_id  TEXT NOT NULL,
				alias      TEXT NOT NULL,
				message_id TEXT NOT NULL,
				PRIMARY KEY (login_id, portal_id, alias)
			)`)
		if err != nil {
			return err
		}
		_, err = db.Exec(ctx, `CREATE INDEX xmpp_message_alias_message_idx ON xmpp_message_alias (login_id, portal_id, message_id)`)
		return err
	}).
	Finish()

func newAliasDB(db *dbutil.Database, log zerolog.Logger) *aliasDB {
	return &aliasDB{Database: db.Child("xmpp_version", aliasUpgradeTable, dbutil.ZeroLogger(log))}
}

func (a *aliasDB) Put(ctx context.Context, login networkid.UserLoginID, portal networkid.PortalID, alias string, msg networkid.MessageID) error {
	if alias == "" {
		return nil
	}
	_, err := a.Exec(ctx, `
		INSERT INTO xmpp_message_alias (login_id, portal_id, alias, message_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (login_id, portal_id, alias) DO UPDATE SET message_id=excluded.message_id`,
		login, portal, alias, msg)
	return err
}

func (a *aliasDB) Get(ctx context.Context, login networkid.UserLoginID, portal networkid.PortalID, alias string) (networkid.MessageID, error) {
	var msg networkid.MessageID
	err := a.QueryRow(ctx, `SELECT message_id FROM xmpp_message_alias WHERE login_id=$1 AND portal_id=$2 AND alias=$3`,
		login, portal, alias).Scan(&msg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return msg, err
}

func (a *aliasDB) DeleteLogin(ctx context.Context, login networkid.UserLoginID) error {
	_, err := a.Exec(ctx, `DELETE FROM xmpp_message_alias WHERE login_id=$1`, login)
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GetAlias returns the stanza-id alias of a bridged message, if any.
func (a *aliasDB) GetAlias(ctx context.Context, login networkid.UserLoginID, portal networkid.PortalID, msg networkid.MessageID) (string, error) {
	var alias string
	err := a.QueryRow(ctx, `SELECT alias FROM xmpp_message_alias WHERE login_id=$1 AND portal_id=$2 AND message_id=$3 LIMIT 1`,
		login, portal, msg).Scan(&alias)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return alias, err
}
