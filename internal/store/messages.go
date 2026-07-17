package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store/storedb"
)

type UpsertMessageParams struct {
	ChatJID         string
	ChatName        string
	MsgID           string
	SenderJID       string
	SenderName      string
	Timestamp       time.Time
	FromMe          bool
	Text            string
	DisplayText     string
	QuotedMsgID     string
	QuotedSenderJID string
	Buttons         []Button
	IsForwarded     bool
	ForwardingScore uint32
	ReactionToID    string
	ReactionEmoji   string
	MediaType       string
	MediaCaption    string
	Filename        string
	MimeType        string
	DirectPath      string
	MediaKey        []byte
	FileSHA256      []byte
	FileEncSHA256   []byte
	FileLength      uint64
	Edited          bool
	Revoked         bool
	DeletedForMe    bool
	Origin          string
	// TRI-STATE: nil = we could not derive it. Never persisted as a bare false.
	MentionsMe  *bool
	RepliesToMe *bool
}

func messageSelectColumns(snippet string) string {
	return fmt.Sprintf(`m.rowid, m.chat_jid, COALESCE(c.name,''), m.msg_id, COALESCE(m.sender_jid,''), COALESCE(m.sender_name,''), m.ts, m.from_me, COALESCE(m.text,''), COALESCE(m.display_text,''), COALESCE(m.quoted_msg_id,''), COALESCE(m.quoted_sender_jid,''), m.mentions_me, m.replies_to_me, m.is_forwarded, m.forwarding_score, COALESCE(m.reaction_to_id,''), COALESCE(m.reaction_emoji,''), COALESCE(m.media_type,''), COALESCE(m.media_caption,''), COALESCE(m.filename,''), COALESCE(m.mime_type,''), COALESCE(m.direct_path,''), COALESCE(m.local_path,''), COALESCE(m.downloaded_at,0), CASE WHEN s.msg_id IS NULL THEN 0 ELSE 1 END, COALESCE(s.starred_at,0), m.revoked, m.deleted_for_me, COALESCE(m.buttons,''), %s`, snippetSQL(snippet))
}

func snippetSQL(snippet string) string {
	if strings.TrimSpace(snippet) == "" {
		return "''"
	}
	return snippet
}

func (d *DB) UpsertMessage(p UpsertMessageParams) error {
	origin, err := normalizeMessageOrigin(p.Origin)
	if err != nil {
		return err
	}
	params := prepareUpsertMessage(p, origin)

	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireMessageWriter(tx); err != nil {
		return err
	}

	prior, err := readMessageChangeState(tx, p.ChatJID, p.MsgID)
	priorExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := d.q.WithTx(tx).UpsertMessage(storeCtx(), params); err != nil {
		return err
	}
	current, err := readMessageChangeState(tx, p.ChatJID, p.MsgID)
	if err != nil {
		return err
	}

	kind := ""
	switch {
	case !priorExists:
		kind = "insert"
	// Revoke/delete transitions outrank the history->live upgrade: a live
	// revoke over a history-origin row still flips ingest_origin, but its
	// change kind must stay 'revoke' — emitting it as insert(origin='live')
	// would push a blank tombstone into the consumer's forwardable lane.
	case !prior.revoked && current.revoked:
		kind = "revoke"
	case !prior.deletedForMe && current.deletedForMe:
		kind = "delete"
	case prior.ingestOrigin == "history" && origin == "live" && current.ingestOrigin == "live":
		kind = "insert"
	case prior.ingestOrigin == "live" && origin == "history":
		// History must never supersede a durable live delivery in the stream.
	case prior.text != current.text || prior.displayText != current.displayText || (!prior.edited && current.edited) || prior.editedTS != current.editedTS:
		kind = "edit"
	}
	if kind != "" {
		if err := appendMessageChange(tx, kind, origin, current); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func prepareUpsertMessage(p UpsertMessageParams, origin string) storedb.UpsertMessageParams {
	if p.Revoked || p.DeletedForMe {
		p.Text = ""
		p.Buttons = nil
		p.QuotedMsgID = ""
		p.QuotedSenderJID = ""
		if p.DeletedForMe {
			p.DisplayText = DeletedForMeMessageDisplayText
		} else {
			p.DisplayText = DeletedMessageDisplayText
		}
		p.MediaType = ""
		p.MediaCaption = ""
		p.Filename = ""
		p.MimeType = ""
		p.DirectPath = ""
		p.MediaKey = nil
		p.FileSHA256 = nil
		p.FileEncSHA256 = nil
		p.FileLength = 0
	}
	var buttonsJSON sql.NullString
	if len(p.Buttons) > 0 {
		if b, err := json.Marshal(p.Buttons); err == nil {
			buttonsJSON = sql.NullString{String: string(b), Valid: true}
		}
	}
	editedTS := int64(0)
	if p.Edited {
		editedTS = unix(p.Timestamp)
	}
	return storedb.UpsertMessageParams{
		ChatJid:         p.ChatJID,
		ChatName:        nullString(p.ChatName),
		MsgID:           p.MsgID,
		SenderJid:       nullString(p.SenderJID),
		SenderName:      nullString(p.SenderName),
		Ts:              unix(p.Timestamp),
		FromMe:          boolToInt64(p.FromMe),
		Text:            nullString(p.Text),
		DisplayText:     nullString(p.DisplayText),
		QuotedMsgID:     nullString(p.QuotedMsgID),
		QuotedSenderJid: nullString(p.QuotedSenderJID),
		MentionsMe:      boolPtrToNullInt64(p.MentionsMe),
		RepliesToMe:     boolPtrToNullInt64(p.RepliesToMe),
		IsForwarded:     boolToInt64(p.IsForwarded),
		ForwardingScore: int64(p.ForwardingScore),
		ReactionToID:    nullString(p.ReactionToID),
		ReactionEmoji:   nullString(p.ReactionEmoji),
		MediaType:       nullString(p.MediaType),
		MediaCaption:    nullString(p.MediaCaption),
		Filename:        nullString(p.Filename),
		MimeType:        nullString(p.MimeType),
		DirectPath:      nullString(p.DirectPath),
		MediaKey:        p.MediaKey,
		FileSha256:      p.FileSHA256,
		FileEncSha256:   p.FileEncSHA256,
		FileLength:      sqlNullInt64(int64(p.FileLength)),
		Revoked:         boolToInt64(p.Revoked),
		DeletedForMe:    boolToInt64(p.DeletedForMe),
		Edited:          boolToInt64(p.Edited),
		EditedTs:        editedTS,
		Buttons:         buttonsJSON,
		IngestOrigin:    origin,
	}
}

func (d *DB) MarkMessageRevoked(chatJID, msgID string) error {
	chatJID = strings.TrimSpace(chatJID)
	msgID = strings.TrimSpace(msgID)
	return d.mutateExistingMessage(chatJID, msgID, "revoke", func(q *storedb.Queries) (int64, error) {
		return q.MarkMessageRevoked(storeCtx(), storedb.MarkMessageRevokedParams{
			DisplayText: sql.NullString{String: DeletedMessageDisplayText, Valid: true},
			ChatJid:     chatJID,
			MsgID:       msgID,
		})
	})
}

func (d *DB) MarkMessageDeletedForMe(chatJID, msgID, senderJID string, fromMe bool, deletedAt time.Time) error {
	chatJID = strings.TrimSpace(chatJID)
	msgID = strings.TrimSpace(msgID)
	if chatJID == "" {
		return fmt.Errorf("chat JID is required")
	}
	if msgID == "" {
		return fmt.Errorf("message ID is required")
	}
	if deletedAt.IsZero() {
		deletedAt = nowUTC()
	}
	err := d.mutateExistingMessage(chatJID, msgID, "delete", func(q *storedb.Queries) (int64, error) {
		return q.MarkMessageDeletedForMe(storeCtx(), storedb.MarkMessageDeletedForMeParams{
			DisplayText: sql.NullString{String: DeletedForMeMessageDisplayText, Valid: true},
			ChatJid:     chatJID,
			MsgID:       msgID,
		})
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Deliberately NON-emitting: a delete-for-me for a message the store never
	// held inserts a contentless tombstone row only. There is nothing a change
	// consumer could act on — no content existed at any seq — and an
	// insert-kind change here would push a blank row into the forwardable lane.
	params := prepareUpsertMessage(UpsertMessageParams{
		ChatJID:      chatJID,
		MsgID:        msgID,
		SenderJID:    senderJID,
		Timestamp:    deletedAt,
		FromMe:       fromMe,
		DeletedForMe: true,
	}, "live")
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.q.WithTx(tx).UpsertMessage(storeCtx(), params); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) MarkMessageDeletedForMePreserveMedia(chatJID, msgID string) error {
	chatJID = strings.TrimSpace(chatJID)
	msgID = strings.TrimSpace(msgID)
	if chatJID == "" {
		return fmt.Errorf("chat JID is required")
	}
	if msgID == "" {
		return fmt.Errorf("message ID is required")
	}
	return d.mutateExistingMessage(chatJID, msgID, "delete", func(q *storedb.Queries) (int64, error) {
		return q.MarkMessageDeletedForMePreserveMedia(storeCtx(), storedb.MarkMessageDeletedForMePreserveMediaParams{
			DisplayText: sql.NullString{String: DeletedForMeMessageDisplayText, Valid: true},
			ChatJid:     chatJID,
			MsgID:       msgID,
		})
	})
}

func (d *DB) UpdateMessageText(chatJID, msgID, text string) error {
	chatJID = strings.TrimSpace(chatJID)
	msgID = strings.TrimSpace(msgID)
	return d.mutateExistingMessage(chatJID, msgID, "edit", func(q *storedb.Queries) (int64, error) {
		return q.UpdateMessageText(storeCtx(), storedb.UpdateMessageTextParams{
			Text:        nullString(text),
			DisplayText: nullString(text),
			ChatJid:     chatJID,
			MsgID:       msgID,
		})
	})
}

type messageChangeState struct {
	chatJID      string
	msgID        string
	text         string
	displayText  string
	revoked      bool
	deletedForMe bool
	edited       bool
	editedTS     int64
	ingestOrigin string
	ts           int64
	fromMe       bool
}

func acquireMessageWriter(tx *sql.Tx) error {
	// UpsertMessage must read prior state before writing. A no-op write first
	// acquires SQLite's single writer slot, so the read snapshot cannot lose a
	// later write-upgrade race to concurrent media metadata updates.
	_, err := tx.ExecContext(storeCtx(), `UPDATE store_meta SET value = value WHERE 0`)
	return err
}

func normalizeMessageOrigin(origin string) (string, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return "live", nil
	}
	if origin != "live" && origin != "history" {
		return "", fmt.Errorf("message origin must be live or history")
	}
	return origin, nil
}

func readMessageChangeState(tx *sql.Tx, chatJID, msgID string) (messageChangeState, error) {
	var state messageChangeState
	var revoked, deletedForMe, edited, fromMe int
	err := tx.QueryRowContext(storeCtx(), `
		SELECT chat_jid, msg_id, COALESCE(text,''), COALESCE(display_text,''),
		       revoked, deleted_for_me, edited, edited_ts, ingest_origin, ts, from_me
		FROM messages
		WHERE chat_jid = ? AND msg_id = ?
	`, chatJID, msgID).Scan(
		&state.chatJID, &state.msgID, &state.text, &state.displayText,
		&revoked, &deletedForMe, &edited, &state.editedTS, &state.ingestOrigin, &state.ts, &fromMe,
	)
	if err != nil {
		return messageChangeState{}, err
	}
	state.revoked = revoked != 0
	state.deletedForMe = deletedForMe != 0
	state.edited = edited != 0
	state.fromMe = fromMe != 0
	return state, nil
}

func appendMessageChange(tx *sql.Tx, kind, origin string, state messageChangeState) error {
	_, err := tx.ExecContext(storeCtx(), `
		INSERT INTO message_changes(chat_jid, msg_id, kind, origin, ts, from_me, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`, state.chatJID, state.msgID, kind, origin, state.ts, boolToInt(state.fromMe), nowUTC().Unix())
	return err
}

func (d *DB) mutateExistingMessage(chatJID, msgID, kind string, mutate func(*storedb.Queries) (int64, error)) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := mutate(d.q.WithTx(tx))
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	state, err := readMessageChangeState(tx, chatJID, msgID)
	if err != nil {
		return err
	}
	if err := appendMessageChange(tx, kind, "live", state); err != nil {
		return err
	}
	return tx.Commit()
}

type ListMessagesParams struct {
	ChatJID   string
	ChatJIDs  []string
	SenderJID string
	Limit     int
	Before    *time.Time
	After     *time.Time
	FromMe    *bool
	Asc       bool
	Forwarded bool
	Starred   bool
}

func (d *DB) ListMessages(p ListMessagesParams) ([]Message, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	query := `
		SELECT ` + messageSelectColumns("") + `
		FROM messages m
		LEFT JOIN chats c ON c.jid = m.chat_jid
		LEFT JOIN starred s ON s.chat_jid = m.chat_jid AND s.msg_id = m.msg_id
		WHERE m.revoked = 0 AND m.deleted_for_me = 0`
	var args []interface{}
	query, args = appendStringFilter(query, args, "m.chat_jid", p.ChatJID, p.ChatJIDs)
	if p.After != nil {
		query += " AND m.ts > ?"
		args = append(args, unix(*p.After))
	}
	if p.Before != nil {
		query += " AND m.ts < ?"
		args = append(args, unix(*p.Before))
	}
	if strings.TrimSpace(p.SenderJID) != "" {
		query += " AND m.sender_jid = ?"
		args = append(args, strings.TrimSpace(p.SenderJID))
	}
	if p.FromMe != nil {
		query += " AND m.from_me = ?"
		args = append(args, boolToInt(*p.FromMe))
	}
	if p.Forwarded {
		query += " AND m.is_forwarded = 1"
	}
	if p.Starred {
		query += " AND s.msg_id IS NOT NULL"
	}
	if p.Asc {
		query += " ORDER BY m.ts ASC, m.rowid ASC LIMIT ?"
	} else {
		query += " ORDER BY m.ts DESC, m.rowid DESC LIMIT ?"
	}
	args = append(args, p.Limit)
	return d.scanMessages(query, args...)
}

func appendStringFilter(query string, args []interface{}, column, value string, values []string) (string, []interface{}) {
	filterValues := uniqueNonEmptyStrings(append([]string{value}, values...))
	switch len(filterValues) {
	case 0:
		return query, args
	case 1:
		query += " AND " + column + " = ?"
		args = append(args, filterValues[0])
		return query, args
	default:
		query += " AND " + column + " IN (" + strings.TrimRight(strings.Repeat("?,", len(filterValues)), ",") + ")"
		for _, v := range filterValues {
			args = append(args, v)
		}
		return query, args
	}
}

func uniqueNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (d *DB) GetMessage(chatJID, msgID string) (Message, error) {
	row, err := d.q.GetMessage(storeCtx(), storedb.GetMessageParams{ChatJid: chatJID, MsgID: msgID})
	if err != nil {
		return Message{}, err
	}
	return messageFromGetRow(row), nil
}

func (d *DB) CountMessages() (int64, error) {
	return d.q.CountMessages(storeCtx())
}

func (d *DB) GetOldestMessageInfo(chatJID string) (MessageInfo, error) {
	chatJID = strings.TrimSpace(chatJID)
	if chatJID == "" {
		return MessageInfo{}, fmt.Errorf("chat JID is required")
	}
	row, err := d.q.GetOldestMessageInfo(storeCtx(), chatJID)
	if err != nil {
		return MessageInfo{}, err
	}
	return messageInfoFromOldestRow(row), nil
}

func (d *DB) GetLatestMessageInfo(chatJID string) (MessageInfo, error) {
	chatJID = strings.TrimSpace(chatJID)
	if chatJID == "" {
		return MessageInfo{}, fmt.Errorf("chat JID is required")
	}
	row, err := d.q.GetLatestMessageInfo(storeCtx(), chatJID)
	if err != nil {
		return MessageInfo{}, err
	}
	return messageInfoFromLatestRow(row), nil
}

func (d *DB) MessageContext(chatJID, msgID string, before, after int) ([]Message, error) {
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	target, err := d.GetMessage(chatJID, msgID)
	if err != nil {
		return nil, err
	}

	beforeRows, err := d.q.MessageContextBefore(storeCtx(), storedb.MessageContextBeforeParams{
		ChatJid: chatJID,
		Ts:      unix(target.Timestamp),
		Ts_2:    unix(target.Timestamp),
		Rowid:   target.rowID,
		Limit:   int64(before),
	})
	if err != nil {
		return nil, err
	}

	afterRows, err := d.q.MessageContextAfter(storeCtx(), storedb.MessageContextAfterParams{
		ChatJid: chatJID,
		Ts:      unix(target.Timestamp),
		Ts_2:    unix(target.Timestamp),
		Rowid:   target.rowID,
		Limit:   int64(after),
	})
	if err != nil {
		return nil, err
	}
	beforeMessages := make([]Message, 0, len(beforeRows))
	for _, row := range beforeRows {
		beforeMessages = append(beforeMessages, messageFromBeforeRow(row))
	}
	afterMessages := make([]Message, 0, len(afterRows))
	for _, row := range afterRows {
		afterMessages = append(afterMessages, messageFromAfterRow(row))
	}

	// Reverse before rows back to chronological order.
	for i, j := 0, len(beforeMessages)-1; i < j; i, j = i+1, j-1 {
		beforeMessages[i], beforeMessages[j] = beforeMessages[j], beforeMessages[i]
	}

	out := make([]Message, 0, len(beforeMessages)+1+len(afterMessages))
	out = append(out, beforeMessages...)
	out = append(out, target)
	out = append(out, afterMessages...)
	return out, nil
}

func (d *DB) scanMessages(query string, args ...interface{}) ([]Message, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
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
		var mentionsMe sql.NullInt64
		var repliesToMe sql.NullInt64
		if err := rows.Scan(&m.rowID, &m.ChatJID, &m.ChatName, &m.MsgID, &m.SenderJID, &m.SenderName, &ts, &fromMe, &m.Text, &m.DisplayText, &m.QuotedMsgID, &m.QuotedSenderJID, &mentionsMe, &repliesToMe, &forwarded, &forwardingScore, &m.ReactionToID, &m.ReactionEmoji, &m.MediaType, &m.MediaCaption, &m.Filename, &m.MimeType, &m.DirectPath, &m.LocalPath, &downloadedAt, &starred, &starredAt, &revoked, &deletedForMe, &buttonsJSON, &m.Snippet); err != nil {
			return nil, err
		}
		m.Timestamp = fromUnix(ts)
		// NULL stays nil (unknown), never false. SQLite has no bool: 0/1 -> *bool.
		m.MentionsMe = nullInt64ToBoolPtr(mentionsMe)
		m.RepliesToMe = nullInt64ToBoolPtr(repliesToMe)
		m.FromMe = fromMe != 0
		m.IsForwarded = forwarded != 0
		m.ForwardingScore = uint32(forwardingScore)
		m.DownloadedAt = fromUnix(downloadedAt)
		m.Starred = starred != 0
		m.StarredAt = fromUnix(starredAt)
		m.Revoked = revoked != 0
		m.DeletedForMe = deletedForMe != 0
		if buttonsJSON != "" {
			_ = json.Unmarshal([]byte(buttonsJSON), &m.Buttons)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func messageFromGetRow(row storedb.GetMessageRow) Message {
	return messageFromScalars(
		row.Rowid, row.ChatJid, row.Name, row.MsgID, row.SenderJid, row.SenderName,
		row.Ts, row.FromMe, row.Text, row.DisplayText, row.QuotedMsgID, row.QuotedSenderJid, row.IsForwarded,
		row.ForwardingScore, row.ReactionToID, row.ReactionEmoji, row.MediaType,
		row.MediaCaption, row.Filename, row.MimeType, row.DirectPath, row.LocalPath,
		row.DownloadedAt, row.Column24, row.StarredAt, row.Revoked, row.DeletedForMe,
		row.Buttons, row.Column29,
	)
}

func messageFromBeforeRow(row storedb.MessageContextBeforeRow) Message {
	return messageFromScalars(
		row.Rowid, row.ChatJid, row.Name, row.MsgID, row.SenderJid, row.SenderName,
		row.Ts, row.FromMe, row.Text, row.DisplayText, row.QuotedMsgID, row.QuotedSenderJid, row.IsForwarded,
		row.ForwardingScore, row.ReactionToID, row.ReactionEmoji, row.MediaType,
		row.MediaCaption, row.Filename, row.MimeType, row.DirectPath, row.LocalPath,
		row.DownloadedAt, row.Column24, row.StarredAt, row.Revoked, row.DeletedForMe,
		row.Buttons, row.Column29,
	)
}

func messageFromAfterRow(row storedb.MessageContextAfterRow) Message {
	return messageFromScalars(
		row.Rowid, row.ChatJid, row.Name, row.MsgID, row.SenderJid, row.SenderName,
		row.Ts, row.FromMe, row.Text, row.DisplayText, row.QuotedMsgID, row.QuotedSenderJid, row.IsForwarded,
		row.ForwardingScore, row.ReactionToID, row.ReactionEmoji, row.MediaType,
		row.MediaCaption, row.Filename, row.MimeType, row.DirectPath, row.LocalPath,
		row.DownloadedAt, row.Column24, row.StarredAt, row.Revoked, row.DeletedForMe,
		row.Buttons, row.Column29,
	)
}

func messageFromScalars(rowID int64, chatJID, chatName, msgID, senderJID, senderName string, ts, fromMe int64, text, displayText, quotedMsgID, quotedSenderJID string, forwarded, forwardingScore int64, reactionToID, reactionEmoji, mediaType, mediaCaption, filename, mimeType, directPath, localPath string, downloadedAt, starred, starredAt, revoked, deletedForMe int64, buttonsJSON, snippet string) Message {
	m := Message{
		rowID:           rowID,
		ChatJID:         chatJID,
		ChatName:        chatName,
		MsgID:           msgID,
		SenderJID:       senderJID,
		SenderName:      senderName,
		Timestamp:       fromUnix(ts),
		FromMe:          fromMe != 0,
		Text:            text,
		DisplayText:     displayText,
		QuotedMsgID:     quotedMsgID,
		QuotedSenderJID: quotedSenderJID,
		IsForwarded:     forwarded != 0,
		ForwardingScore: uint32(forwardingScore),
		ReactionToID:    reactionToID,
		ReactionEmoji:   reactionEmoji,
		MediaType:       mediaType,
		MediaCaption:    mediaCaption,
		Filename:        filename,
		MimeType:        mimeType,
		DirectPath:      directPath,
		LocalPath:       localPath,
		DownloadedAt:    fromUnix(downloadedAt),
		Starred:         starred != 0,
		StarredAt:       fromUnix(starredAt),
		Revoked:         revoked != 0,
		DeletedForMe:    deletedForMe != 0,
		Snippet:         snippet,
	}
	if buttonsJSON != "" {
		_ = json.Unmarshal([]byte(buttonsJSON), &m.Buttons)
	}
	return m
}

func messageInfoFromOldestRow(row storedb.GetOldestMessageInfoRow) MessageInfo {
	return MessageInfo{
		ChatJID:    row.ChatJid,
		MsgID:      row.MsgID,
		Timestamp:  fromUnix(row.Ts),
		FromMe:     row.FromMe != 0,
		SenderJID:  row.SenderJid,
		SenderName: row.SenderName,
	}
}

func messageInfoFromLatestRow(row storedb.GetLatestMessageInfoRow) MessageInfo {
	return MessageInfo{
		ChatJID:    row.ChatJid,
		MsgID:      row.MsgID,
		Timestamp:  fromUnix(row.Ts),
		FromMe:     row.FromMe != 0,
		SenderJID:  row.SenderJid,
		SenderName: row.SenderName,
	}
}

// nullInt64ToBoolPtr maps a nullable SQLite integer to a tri-state bool.
//
// NULL -> nil (unknown), NOT false. Collapsing NULL to false would assert "you were not
// addressed" about a message we never managed to examine.
func nullInt64ToBoolPtr(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// boolPtrToNullInt64 maps a tri-state bool to a nullable SQLite integer (nil -> NULL).
func boolPtrToNullInt64(v *bool) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	n := int64(0)
	if *v {
		n = 1
	}
	return sql.NullInt64{Int64: n, Valid: true}
}

// MessageAuthorship reports whether we hold a local record of a message, and whether WE wrote it.
//
// This is the proof behind RepliesToMe (AITOOLS-927). ContextInfo.participant is an attacker-supplied
// CLAIM that Wave authored the quoted message; only our own store can corroborate it. Keyed on
// (chat_jid, msg_id) — never msg_id alone, or the same id in a DIFFERENT chat would forge the proof.
//
// found=false means "no record", which the caller must treat as UNRESOLVED (null) — never as a
// confirmed authorship and never as a confirmed denial. (Unresolved is not "held for replay": no
// durable hold exists yet.)
func (d *DB) MessageAuthorship(chatJID, msgID string) (found bool, fromMe bool, err error) {
	chatJID = strings.TrimSpace(chatJID)
	msgID = strings.TrimSpace(msgID)
	if chatJID == "" || msgID == "" {
		return false, false, nil
	}
	var fm int64
	err = d.sql.QueryRow(
		`SELECT from_me FROM messages WHERE chat_jid = ? AND msg_id = ?`, chatJID, msgID,
	).Scan(&fm)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, fm != 0, nil
}
