package store

import (
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrChangeCursorGap    = errors.New("cursor_gap")
	ErrChangeCursorFuture = errors.New("cursor_future")
	ErrStoreNotMigrated   = errors.New("store_not_migrated")
)

type MessageChange struct {
	Seq     int64    `json:"seq"`
	Kind    string   `json:"kind"`
	Origin  string   `json:"origin"`
	ChatJID string   `json:"chat_jid"`
	MsgID   string   `json:"msg_id"`
	TS      int64    `json:"ts"`
	FromMe  bool     `json:"from_me"`
	Message *Message `json:"message"`
}

type MessageChangesPage struct {
	StoreInstanceID string          `json:"store_instance_id"`
	MinSeq          int64           `json:"min_seq"`
	LatestSeq       int64           `json:"latest_seq"`
	Changes         []MessageChange `json:"changes"`
	Purged          int             `json:"purged"`
}

type MessageChangesStatus struct {
	StoreInstanceID string `json:"store_instance_id"`
	MinSeq          int64  `json:"min_seq"`
	LatestSeq       int64  `json:"latest_seq"`
	MaxAllocated    int64  `json:"max_allocated"`
	BootstrapSeq    int64  `json:"bootstrap_seq"`
}

type messageChangeBounds struct {
	storeInstanceID string
	lastPruned      int64
	maxAllocated    int64
	minSeq          int64
	latestSeq       int64
}

func readMessageChangeBounds(tx *sql.Tx) (messageChangeBounds, error) {
	var bounds messageChangeBounds
	err := tx.QueryRowContext(storeCtx(), `
		SELECT
			(SELECT value FROM store_meta WHERE key = 'store_instance_id'),
			CAST((SELECT value FROM store_meta WHERE key = 'changes_last_pruned_seq') AS INTEGER),
			COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'message_changes'), 0),
			COALESCE((SELECT MIN(seq) FROM message_changes), 0),
			COALESCE((SELECT MAX(seq) FROM message_changes), 0)
	`).Scan(
		&bounds.storeInstanceID,
		&bounds.lastPruned,
		&bounds.maxAllocated,
		&bounds.minSeq,
		&bounds.latestSeq,
	)
	return bounds, err
}

func requireMessageChangesSchema(tx *sql.Tx) error {
	var tables int
	if err := tx.QueryRowContext(storeCtx(), `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name IN ('store_meta', 'message_changes')
	`).Scan(&tables); err != nil {
		return err
	}
	if tables != 2 {
		return ErrStoreNotMigrated
	}

	var messageColumns, changeColumns, metadata int
	if err := tx.QueryRowContext(storeCtx(), `
		SELECT
			(SELECT COUNT(*) FROM pragma_table_info('messages')
			 WHERE name IN ('mentions_me', 'replies_to_me')),
			(SELECT COUNT(*) FROM pragma_table_info('message_changes')
			 WHERE name IN ('seq', 'chat_jid', 'msg_id', 'kind', 'origin', 'ts', 'from_me', 'created_at')),
			(SELECT COUNT(*) FROM store_meta
			 WHERE key IN ('store_instance_id', 'changes_last_pruned_seq'))
	`).Scan(&messageColumns, &changeColumns, &metadata); err != nil {
		return err
	}
	if messageColumns != 2 {
		return ErrStoreNotMigrated
	}
	if changeColumns != 8 {
		return ErrStoreNotMigrated
	}
	if metadata != 2 {
		return ErrStoreNotMigrated
	}
	return nil
}

func (d *DB) ListMessageChanges(afterSeq int64, limit int) (MessageChangesPage, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	tx, err := d.sql.BeginTx(storeCtx(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MessageChangesPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireMessageChangesSchema(tx); err != nil {
		return MessageChangesPage{}, err
	}

	bounds, err := readMessageChangeBounds(tx)
	if err != nil {
		return MessageChangesPage{}, err
	}
	if afterSeq < bounds.lastPruned {
		return MessageChangesPage{}, ErrChangeCursorGap
	}
	if afterSeq > bounds.maxAllocated {
		return MessageChangesPage{}, ErrChangeCursorFuture
	}

	rows, err := tx.QueryContext(storeCtx(), `
		SELECT
			mc.seq, mc.kind, mc.origin, mc.chat_jid, mc.msg_id, mc.ts, mc.from_me,
			CASE WHEN m.rowid IS NULL THEN 0 ELSE 1 END,
			COALESCE(m.rowid,0), COALESCE(m.chat_jid,''), COALESCE(c.name,''), COALESCE(m.msg_id,''),
			COALESCE(m.sender_jid,''), COALESCE(m.sender_name,''), COALESCE(m.ts,0), COALESCE(m.from_me,0),
			COALESCE(m.text,''), COALESCE(m.display_text,''), COALESCE(m.quoted_msg_id,''), COALESCE(m.quoted_sender_jid,''),
			m.mentions_me, m.replies_to_me,
			COALESCE(m.is_forwarded,0), COALESCE(m.forwarding_score,0), COALESCE(m.reaction_to_id,''), COALESCE(m.reaction_emoji,''),
			COALESCE(m.media_type,''), COALESCE(m.media_caption,''), COALESCE(m.filename,''), COALESCE(m.mime_type,''),
			COALESCE(m.direct_path,''), COALESCE(m.local_path,''), COALESCE(m.downloaded_at,0),
			CASE WHEN s.msg_id IS NULL THEN 0 ELSE 1 END, COALESCE(s.starred_at,0),
			COALESCE(m.revoked,0), COALESCE(m.deleted_for_me,0), COALESCE(m.buttons,''), ''
		FROM message_changes mc
		LEFT JOIN messages m ON m.chat_jid = mc.chat_jid AND m.msg_id = mc.msg_id
		LEFT JOIN chats c ON c.jid = m.chat_jid
		LEFT JOIN starred s ON s.chat_jid = m.chat_jid AND s.msg_id = m.msg_id
		WHERE mc.seq > ?
		ORDER BY mc.seq ASC
		LIMIT ?
	`, afterSeq, limit)
	if err != nil {
		return MessageChangesPage{}, err
	}
	defer rows.Close()

	page := MessageChangesPage{
		StoreInstanceID: bounds.storeInstanceID,
		MinSeq:          bounds.minSeq,
		LatestSeq:       bounds.latestSeq,
		Changes:         make([]MessageChange, 0),
	}
	for rows.Next() {
		change, present, message, err := scanMessageChange(rows)
		if err != nil {
			return MessageChangesPage{}, err
		}
		if present {
			change.Message = &message
		} else {
			page.Purged++
		}
		page.Changes = append(page.Changes, change)
	}
	if err := rows.Err(); err != nil {
		return MessageChangesPage{}, err
	}
	if err := rows.Close(); err != nil {
		return MessageChangesPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageChangesPage{}, err
	}
	return page, nil
}

func scanMessageChange(rows *sql.Rows) (MessageChange, bool, Message, error) {
	var change MessageChange
	var changeFromMe int
	var present int
	var m Message
	var ts int64
	var fromMe int
	var forwarded int
	var forwardingScore int64
	var downloadedAt int64
	var starred int
	var starredAt int64
	var revoked int
	var deletedForMe int
	var buttonsJSON string
	var snippet string
	var mentionsMe sql.NullInt64
	var repliesToMe sql.NullInt64
	err := rows.Scan(
		&change.Seq, &change.Kind, &change.Origin, &change.ChatJID, &change.MsgID, &change.TS, &changeFromMe,
		&present,
		&m.rowID, &m.ChatJID, &m.ChatName, &m.MsgID, &m.SenderJID, &m.SenderName, &ts, &fromMe,
		&m.Text, &m.DisplayText, &m.QuotedMsgID, &m.QuotedSenderJID, &mentionsMe, &repliesToMe, &forwarded, &forwardingScore,
		&m.ReactionToID, &m.ReactionEmoji, &m.MediaType, &m.MediaCaption, &m.Filename, &m.MimeType,
		&m.DirectPath, &m.LocalPath, &downloadedAt, &starred, &starredAt, &revoked, &deletedForMe,
		&buttonsJSON, &snippet,
	)
	if err != nil {
		return MessageChange{}, false, Message{}, err
	}
	change.FromMe = changeFromMe != 0
	m = messageFromScalars(
		m.rowID, m.ChatJID, m.ChatName, m.MsgID, m.SenderJID, m.SenderName,
		ts, int64(fromMe), m.Text, m.DisplayText, m.QuotedMsgID, m.QuotedSenderJID,
		mentionsMe, repliesToMe, int64(forwarded), forwardingScore, m.ReactionToID, m.ReactionEmoji, m.MediaType,
		m.MediaCaption, m.Filename, m.MimeType, m.DirectPath, m.LocalPath,
		downloadedAt, int64(starred), starredAt, int64(revoked), int64(deletedForMe), buttonsJSON, snippet,
	)
	return change, present != 0, m, nil
}

func (d *DB) MessageChangesStatus(lookbackSeconds int64) (MessageChangesStatus, error) {
	if lookbackSeconds < 0 {
		return MessageChangesStatus{}, fmt.Errorf("lookback seconds must not be negative")
	}
	tx, err := d.sql.BeginTx(storeCtx(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MessageChangesStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireMessageChangesSchema(tx); err != nil {
		return MessageChangesStatus{}, err
	}
	bounds, err := readMessageChangeBounds(tx)
	if err != nil {
		return MessageChangesStatus{}, err
	}
	threshold := nowUTC().Unix() - lookbackSeconds
	var bootstrapSeq int64
	if err := tx.QueryRowContext(storeCtx(), `
		SELECT COALESCE(
			(SELECT MIN(seq) - 1 FROM message_changes WHERE created_at >= ?),
			?
		)
	`, threshold, bounds.maxAllocated).Scan(&bootstrapSeq); err != nil {
		return MessageChangesStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageChangesStatus{}, err
	}
	return MessageChangesStatus{
		StoreInstanceID: bounds.storeInstanceID,
		MinSeq:          bounds.minSeq,
		LatestSeq:       bounds.latestSeq,
		MaxAllocated:    bounds.maxAllocated,
		BootstrapSeq:    bootstrapSeq,
	}, nil
}

func (d *DB) CountMessageChangesOlderThan(days int) (int64, error) {
	cutoff, err := messageChangesCutoff(days)
	if err != nil {
		return 0, err
	}
	var count int64
	err = d.sql.QueryRowContext(storeCtx(), `SELECT COUNT(*) FROM message_changes WHERE created_at < ?`, cutoff).Scan(&count)
	return count, err
}

func (d *DB) PruneMessageChangesOlderThan(days int) (int64, error) {
	cutoff, err := messageChangesCutoff(days)
	if err != nil {
		return 0, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var maxPruned sql.NullInt64
	if err := tx.QueryRowContext(storeCtx(), `SELECT MAX(seq) FROM message_changes WHERE created_at < ?`, cutoff).Scan(&maxPruned); err != nil {
		return 0, err
	}
	if !maxPruned.Valid {
		return 0, nil
	}
	result, err := tx.ExecContext(storeCtx(), `DELETE FROM message_changes WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if deleted == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(storeCtx(), `
		INSERT INTO store_meta(key, value) VALUES('changes_last_pruned_seq', CAST(? AS TEXT))
		ON CONFLICT(key) DO UPDATE SET value = CAST(
			CASE
				WHEN CAST(store_meta.value AS INTEGER) > CAST(excluded.value AS INTEGER) THEN CAST(store_meta.value AS INTEGER)
				ELSE excluded.value
			END AS TEXT
		)
	`, maxPruned.Int64); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func messageChangesCutoff(days int) (int64, error) {
	if days <= 0 {
		return 0, fmt.Errorf("message change retention days must be positive")
	}
	return nowUTC().AddDate(0, 0, -days).Unix(), nil
}
