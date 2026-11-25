package budgetstore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// parsedRow is one source row as a parser understood it. Raw keeps every
// column exactly as the file had it.
type parsedRow struct {
	rowNumber   int
	raw         map[string]string
	occurredOn  string
	description string
	amountCents int64
	status      string
	// internal: a move inside the account that changes no balance, such as a
	// cash sweep. Kept as a row, never made a transaction.
	internal bool
}

// sourceParsers read one file format each. The amount a parser returns is
// the change in the account's balance, in this store's sign.
var sourceParsers = map[string]func(header []string, records [][]string) ([]parsedRow, error){
	"wells_fargo_csv": parseWellsFargoCSV,
	"inspira_csv":     parseInspiraCSV,
}

// parseCents reads "-1,234.56" exactly, without floating point.
func parseCents(value string) (int64, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), ",", "")
	value = strings.TrimPrefix(value, "$")
	if value == "" {
		return 0, errors.New("empty amount")
	}
	negative := false
	if strings.HasPrefix(value, "-") {
		negative = true
		value = value[1:]
	} else if strings.HasPrefix(value, "(") && strings.HasSuffix(value, ")") {
		negative = true
		value = value[1 : len(value)-1]
	}
	whole, fraction, _ := strings.Cut(value, ".")
	if len(fraction) > 2 {
		return 0, fmt.Errorf("amount %q has more than two decimal places", value)
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	if whole == "" {
		whole = "0"
	}
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", value, err)
	}
	cents, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", value, err)
	}
	total := dollars*100 + cents
	if negative {
		total = -total
	}
	return total, nil
}

func usDate(value string) (string, error) {
	datePart, _, _ := strings.Cut(strings.TrimSpace(value), " ")
	parsed, err := time.Parse("01/02/2006", datePart)
	if err != nil {
		return "", fmt.Errorf("date %q is not MM/DD/YYYY", value)
	}
	return parsed.Format("2006-01-02"), nil
}

func columnIndex(header []string, names ...string) (map[string]int, error) {
	index := map[string]int{}
	for i, column := range header {
		index[strings.TrimSpace(strings.TrimPrefix(column, "\uFEFF"))] = i
	}
	for _, name := range names {
		if _, found := index[name]; !found {
			return nil, fmt.Errorf("%w: the header has no %q column (it has %v)", ErrInvalid, name, header)
		}
	}
	return index, nil
}

func rawRow(header, record []string) map[string]string {
	raw := map[string]string{}
	for i, column := range header {
		if i < len(record) {
			raw[strings.TrimSpace(strings.TrimPrefix(column, "\uFEFF"))] = record[i]
		}
	}
	return raw
}

// parseWellsFargoCSV reads Wells Fargo's "Download Account Activity" CSV with
// its header: DATE, DESCRIPTION, AMOUNT, CHECK #, STATUS. The amount already
// carries this store's sign for checking and for cards: a purchase is
// negative, a payment to a card is positive.
func parseWellsFargoCSV(header []string, records [][]string) ([]parsedRow, error) {
	index, err := columnIndex(header, "DATE", "DESCRIPTION", "AMOUNT", "STATUS")
	if err != nil {
		return nil, err
	}
	var out []parsedRow
	for i, record := range records {
		row := parsedRow{rowNumber: i + 1, raw: rawRow(header, record)}
		if row.occurredOn, err = usDate(record[index["DATE"]]); err != nil {
			return nil, fmt.Errorf("%w: row %d: %v", ErrInvalid, i+1, err)
		}
		if row.amountCents, err = parseCents(record[index["AMOUNT"]]); err != nil {
			return nil, fmt.Errorf("%w: row %d: %v", ErrInvalid, i+1, err)
		}
		row.description = strings.Join(strings.Fields(record[index["DESCRIPTION"]]), " ")
		switch status := strings.ToLower(strings.TrimSpace(record[index["STATUS"]])); status {
		case "posted":
			row.status = "posted"
		case "pending":
			row.status = "pending"
		default:
			return nil, fmt.Errorf("%w: row %d: unknown STATUS %q", ErrInvalid, i+1, status)
		}
		out = append(out, row)
	}
	return out, nil
}

// inspiraInternalActivityTypes move cash into or out of the sweep fund inside
// the same account: the account's value does not change.
var inspiraInternalActivityTypes = []string{"SWBUY", "SWSELL"}

// parseInspiraCSV reads Inspira Financial's "my transactions" CSV. Amount is
// the change in the account's cash.
func parseInspiraCSV(header []string, records [][]string) ([]parsedRow, error) {
	index, err := columnIndex(header, "Trade Date", "Transaction Type", "Description", "Comment", "Amount", "Activity Type")
	if err != nil {
		return nil, err
	}
	var out []parsedRow
	for i, record := range records {
		row := parsedRow{rowNumber: i + 1, raw: rawRow(header, record), status: "posted"}
		if row.occurredOn, err = usDate(record[index["Trade Date"]]); err != nil {
			return nil, fmt.Errorf("%w: row %d: %v", ErrInvalid, i+1, err)
		}
		if row.amountCents, err = parseCents(record[index["Amount"]]); err != nil {
			return nil, fmt.Errorf("%w: row %d: %v", ErrInvalid, i+1, err)
		}
		parts := []string{strings.TrimSpace(record[index["Transaction Type"]]), strings.TrimSpace(record[index["Description"]])}
		if comment := strings.TrimSpace(record[index["Comment"]]); comment != "" {
			parts = append(parts, comment)
		}
		row.description = strings.Join(parts, ": ")
		row.internal = slices.Contains(inspiraInternalActivityTypes, strings.TrimSpace(record[index["Activity Type"]]))
		out = append(out, row)
	}
	return out, nil
}

// rowDedupeKey identifies a row across files: source, account, date,
// description, amount, and which occurrence it is of identical rows in its
// file, so two real $3 coffees on one day stay two while a re-downloaded
// overlap is caught.
func rowDedupeKey(source, accountID string, row parsedRow, occurrence int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{source, accountID, row.occurredOn, row.description, strconv.FormatInt(row.amountCents, 10), strconv.Itoa(occurrence)}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// ImportBatch is one imported file and what became of its rows.
type ImportBatch struct {
	ID         string         `json:"id"`
	Source     string         `json:"source"`
	AccountID  string         `json:"account_id"`
	FileName   string         `json:"file_name"`
	FileID     string         `json:"file_id"`
	FileSHA256 string         `json:"file_sha256"`
	FirstRowOn string         `json:"first_row_on"`
	LastRowOn  string         `json:"last_row_on"`
	ImportedAt int64          `json:"imported_at"`
	UndoneAt   int64          `json:"undone_at"`
	Outcomes   map[string]int `json:"outcomes"`
	FirstDate  string         `json:"first_date"`
	LastDate   string         `json:"last_date"`
}

// ImportRequest names the file being imported.
type ImportRequest struct {
	Source    string
	AccountID string
	FileName  string
	FileID    string
}

// Import reads a whole file and makes one transaction per new row: the
// account's posting and the opposite amount on the payee's default category
// or on Uncategorized. A row an earlier live batch already holds is kept as
// a duplicate and makes nothing. All or nothing.
func (s *Ledger) Import(request ImportRequest, file io.Reader) (ImportBatch, error) {
	parser, found := sourceParsers[request.Source]
	if !found {
		if err := requireOneOf("source", request.Source, importSources); err != nil {
			return ImportBatch{}, err
		}
		return ImportBatch{}, fmt.Errorf("%w: source %q arrives through its own sync, not as a file", ErrInvalid, request.Source)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return ImportBatch{}, err
	}
	if len(content) == 0 {
		return ImportBatch{}, fmt.Errorf("%w: the file is empty", ErrInvalid)
	}
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return ImportBatch{}, fmt.Errorf("%w: not a CSV: %v", ErrInvalid, err)
	}
	if len(records) < 2 {
		return ImportBatch{}, fmt.Errorf("%w: the file has a header and no rows", ErrInvalid)
	}
	rows, err := parser(records[0], records[1:])
	if err != nil {
		return ImportBatch{}, err
	}
	digest := sha256.Sum256(content)
	fileSHA := hex.EncodeToString(digest[:])

	tx, err := s.db.Begin()
	if err != nil {
		return ImportBatch{}, err
	}
	defer tx.Rollback()
	found, err = exists(tx, "accounts", request.AccountID)
	if err != nil {
		return ImportBatch{}, err
	}
	if !found {
		return ImportBatch{}, fmt.Errorf("%w: account_id %q does not exist", ErrInvalid, request.AccountID)
	}
	var sameFileBatch string
	err = tx.QueryRow(`SELECT id FROM import_batches WHERE file_sha256 = $1 AND account_id = $2 AND undone_at = 0`, fileSHA, request.AccountID).Scan(&sameFileBatch)
	if err == nil {
		return ImportBatch{}, fmt.Errorf("%w: this file is already imported into this account as %s", ErrConflict, sameFileBatch)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ImportBatch{}, err
	}
	firstRowOn, lastRowOn := rows[0].occurredOn, rows[0].occurredOn
	for _, row := range rows {
		firstRowOn, lastRowOn = min(firstRowOn, row.occurredOn), max(lastRowOn, row.occurredOn)
	}
	// A row in this file can be the other side of a moved transfer dated up to
	// transferWindowDays outside the file's own dates.
	first, err := time.Parse("2006-01-02", firstRowOn)
	if err != nil {
		return ImportBatch{}, err
	}
	last, err := time.Parse("2006-01-02", lastRowOn)
	if err != nil {
		return ImportBatch{}, err
	}
	var blocking []string
	blockRows, err := tx.Query(`SELECT w.transaction_id FROM transfers_without_partner w JOIN transactions t ON t.id = w.transaction_id
		WHERE w.account_id = $1 AND t.occurred_on BETWEEN $2 AND $3 ORDER BY t.occurred_on`, request.AccountID,
		first.AddDate(0, 0, -transferWindowDays).Format("2006-01-02"), last.AddDate(0, 0, transferWindowDays).Format("2006-01-02"))
	if err != nil {
		return ImportBatch{}, err
	}
	for blockRows.Next() {
		var id string
		if err := blockRows.Scan(&id); err != nil {
			blockRows.Close()
			return ImportBatch{}, err
		}
		blocking = append(blocking, id)
	}
	blockRows.Close()
	if len(blocking) > 0 {
		return ImportBatch{}, fmt.Errorf("%w: this file covers %s to %s, and the transfer matcher already moved %d transfers onto this account in that range with no partner row (%s); undo them with POST /transactions/{id}/undo-transfer-without-partner, import, then match again",
			ErrConflict, firstRowOn, lastRowOn, len(blocking), strings.Join(blocking, ", "))
	}
	uncategorized, err := categoryIDForRole(tx, "uncategorized")
	if err != nil {
		return ImportBatch{}, err
	}
	rules, err := loadPayeeRules(tx)
	if err != nil {
		return ImportBatch{}, err
	}
	batchSeq, batchID, err := mintID(tx, "import_batches", "import_batch")
	if err != nil {
		return ImportBatch{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO import_batches (seq, id, source, account_id, file_name, file_id, file_sha256, first_row_on, last_row_on, imported_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		batchSeq, batchID, request.Source, request.AccountID, request.FileName, request.FileID, fileSHA, firstRowOn, lastRowOn, now); err != nil {
		return ImportBatch{}, err
	}
	occurrences := map[string]int{}
	for _, row := range rows {
		identity := row.occurredOn + "\x1f" + row.description + "\x1f" + strconv.FormatInt(row.amountCents, 10)
		occurrences[identity]++
		key := rowDedupeKey(request.Source, request.AccountID, row, occurrences[identity])
		var liveCopies int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM import_rows r JOIN import_batches b ON b.id = r.import_batch_id
			WHERE r.dedupe_key = $1 AND b.undone_at = 0 AND r.outcome != 'duplicate'`, key).Scan(&liveCopies); err != nil {
			return ImportBatch{}, err
		}
		outcome := "created"
		switch {
		case liveCopies > 0:
			outcome = "duplicate"
		case row.internal:
			outcome = "skipped_internal"
		}
		raw, err := json.Marshal(row.raw)
		if err != nil {
			return ImportBatch{}, err
		}
		rowSeq, rowID, err := mintID(tx, "import_rows", "import_row")
		if err != nil {
			return ImportBatch{}, err
		}
		if _, err := tx.Exec(`INSERT INTO import_rows (seq, id, import_batch_id, row_number, dedupe_key, outcome, raw_json) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			rowSeq, rowID, batchID, row.rowNumber, key, outcome, string(raw)); err != nil {
			return ImportBatch{}, err
		}
		if outcome != "created" {
			continue
		}
		categoryID := uncategorized
		payeeID := ""
		if rule, matched := matchPayee(rules, row.description); matched {
			payeeID = rule.payeeID
			if rule.defaultCategoryID != "" {
				categoryID = rule.defaultCategoryID
			}
		}
		transactionID, err := s.insertTransaction(tx, TransactionInput{
			OccurredOn:  row.occurredOn,
			Status:      row.status,
			PayeeID:     payeeID,
			Description: row.description,
			Postings: []Posting{
				{AccountID: request.AccountID, AmountCents: row.amountCents},
				{CategoryID: categoryID, AmountCents: -row.amountCents},
			},
		})
		if err != nil {
			return ImportBatch{}, fmt.Errorf("row %d: %w", row.rowNumber, err)
		}
		if _, err := tx.Exec(`INSERT INTO transaction_sources (transaction_id, import_row_id) VALUES ($1,$2)`, transactionID, rowID); err != nil {
			return ImportBatch{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ImportBatch{}, err
	}
	return s.GetImportBatch(batchID)
}

// GetImportBatch returns one batch with counts of each row outcome.
func (s *Ledger) GetImportBatch(id string) (ImportBatch, error) {
	var b ImportBatch
	err := s.db.QueryRow(`SELECT id, source, account_id, file_name, file_id, file_sha256, first_row_on, last_row_on, imported_at, undone_at FROM import_batches WHERE id = $1`, id).
		Scan(&b.ID, &b.Source, &b.AccountID, &b.FileName, &b.FileID, &b.FileSHA256, &b.FirstRowOn, &b.LastRowOn, &b.ImportedAt, &b.UndoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportBatch{}, ErrNotFound
	}
	if err != nil {
		return ImportBatch{}, err
	}
	b.Outcomes = map[string]int{}
	rows, err := s.db.Query(`SELECT outcome, COUNT(*) FROM import_rows WHERE import_batch_id = $1 GROUP BY outcome`, id)
	if err != nil {
		return ImportBatch{}, err
	}
	for rows.Next() {
		var outcome string
		var count int
		if err := rows.Scan(&outcome, &count); err != nil {
			rows.Close()
			return ImportBatch{}, err
		}
		b.Outcomes[outcome] = count
	}
	rows.Close()
	err = s.db.QueryRow(`SELECT COALESCE(MIN(t.occurred_on),''), COALESCE(MAX(t.occurred_on),'') FROM transactions t
		JOIN transaction_sources ts ON ts.transaction_id = t.id JOIN import_rows r ON r.id = ts.import_row_id WHERE r.import_batch_id = $1`, id).Scan(&b.FirstDate, &b.LastDate)
	return b, err
}

// ListImportBatches returns every batch, newest first.
func (s *Ledger) ListImportBatches() ([]ImportBatch, error) {
	rows, err := s.db.Query(`SELECT id FROM import_batches ORDER BY seq DESC`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []ImportBatch{}
	for _, id := range ids {
		b, err := s.GetImportBatch(id)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// UndoImportBatch deletes every transaction the batch made and marks it
// undone; its rows stay as a record. It refuses when one of those
// transactions also holds a row from another batch (a paired transfer):
// unpair it first.
func (s *Ledger) UndoImportBatch(id string) (ImportBatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ImportBatch{}, err
	}
	defer tx.Rollback()
	var undoneAt int64
	if err := tx.QueryRow(`SELECT undone_at FROM import_batches WHERE id = $1`, id).Scan(&undoneAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImportBatch{}, ErrNotFound
		}
		return ImportBatch{}, err
	}
	if undoneAt != 0 {
		return ImportBatch{}, fmt.Errorf("%w: batch %s is already undone", ErrConflict, id)
	}
	var shared string
	err = tx.QueryRow(`SELECT ts.transaction_id FROM transaction_sources ts JOIN import_rows r ON r.id = ts.import_row_id
		WHERE r.import_batch_id = $1 AND ts.transaction_id IN (
			SELECT ts2.transaction_id FROM transaction_sources ts2 JOIN import_rows r2 ON r2.id = ts2.import_row_id WHERE r2.import_batch_id != $2)
		LIMIT 1`, id, id).Scan(&shared)
	if err == nil {
		return ImportBatch{}, fmt.Errorf("%w: %s also holds a row from another batch; unpair it before undoing this batch", ErrConflict, shared)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ImportBatch{}, err
	}
	if _, err := tx.Exec(`DELETE FROM transactions WHERE id IN (
		SELECT ts.transaction_id FROM transaction_sources ts JOIN import_rows r ON r.id = ts.import_row_id WHERE r.import_batch_id = $1)`, id); err != nil {
		return ImportBatch{}, err
	}
	if _, err := tx.Exec(`UPDATE import_batches SET undone_at = $1 WHERE id = $2`, s.unixNow(), id); err != nil {
		return ImportBatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportBatch{}, err
	}
	return s.GetImportBatch(id)
}
