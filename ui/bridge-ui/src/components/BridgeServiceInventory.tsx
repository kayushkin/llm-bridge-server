import { useEffect, useRef, useState } from 'react'
import { useBridgeConfig } from '../context'
import { listServices, readDatabaseRows, readDatabaseSchema } from '../servicesClient'
import type { FetchFn } from '../types'
import type {
  DatabaseColumn, DatabaseRowFilter, DatabaseRowsResponse, DatabaseSchemaResponse, DatabaseTable,
  ServiceDatabase, ServiceInventoryEntry, ServiceInventoryResponse,
} from '@kayushkin/llm-bridge-types'
import { timeAgo } from '../utils'
import styles from './BridgeServiceInventory.module.css'

/**
 * Top-level Service inventory page: every service healthcheck watches on this host,
 * whether it is up, the SQLite files its processes hold open, each file's
 * tables with their DDL and row counts, and the newest rows of any table with
 * a few filters.
 *
 * Left, the services. Right, the one picked: its facts, its databases, and —
 * once a database is picked — its schema, with a rows panel under whichever
 * table is opened. Everything is read-only: llm-bridge-server opens each file
 * `mode=ro` and answers a credential column as null, and this page says so
 * in place rather than drawing an empty cell.
 *
 * Health is healthcheck's, never measured here; the process and file facts
 * are whatever /proc said when the server was asked. A refusal from the
 * server is shown in the server's own words.
 */
export function BridgeServiceInventory() {
  const { fetch: fetchFn, basePath } = useBridgeConfig()
  return <ServicesPage fetchFn={fetchFn} basePath={basePath} />
}

const FILTER_OPS: DatabaseRowFilter['op'][] = ['eq', 'ne', 'contains', 'gt', 'gte', 'lt', 'lte', 'null', 'not_null']
const ROW_LIMITS = [20, 50, 100, 200, 500]

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

function statusDotClass(status: string): string {
  if (status === 'up') return styles.dotUp
  if (status === 'down') return styles.dotDown
  return styles.dotUnknown
}

function ServicesPage({ fetchFn, basePath }: { fetchFn: FetchFn; basePath: string }) {
  const [inventory, setInventory] = useState<ServiceInventoryResponse | null>(null)
  const [inventoryError, setInventoryError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [selectedName, setSelectedName] = useState<string | null>(null)
  const [selectedPath, setSelectedPath] = useState<string | null>(null)
  const inventoryTicket = useRef(0)

  const load = (refresh: boolean) => {
    const ticket = ++inventoryTicket.current
    setLoading(true)
    listServices(fetchFn, basePath, refresh).then(result => {
      if (ticket !== inventoryTicket.current) return
      setLoading(false)
      if (result.ok) { setInventory(result.value); setInventoryError(null) } else setInventoryError(result.error)
    })
  }
  useEffect(() => { load(false) }, [fetchFn, basePath]) // eslint-disable-line react-hooks/exhaustive-deps

  const services = inventory?.services ?? []
  const selected = services.find(s => s.name === selectedName) ?? null
  // A database is shown only while the selected service still lists it: after
  // a refresh in which the service closed it, the panel must not keep reading.
  const selectedDatabase = selected?.databases.find(d => d.path === selectedPath) ?? null

  const upCount = services.filter(s => s.status === 'up').length
  const downCount = services.filter(s => s.status === 'down').length

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div className={styles.headerRow}>
          <h2 className={styles.title}>Service inventory</h2>
          {inventory && (
            <span className={styles.muted}>
              {upCount} up · {downCount} down · healthcheck read {timeAgo(inventory.checked_at)}
            </span>
          )}
          <button type="button" className={styles.button} onClick={() => load(true)} disabled={loading}>
            {loading ? 'Reading…' : 'Refresh'}
          </button>
        </div>
        <p className={styles.subtitle}>
          Every service healthcheck watches, the SQLite files its processes hold open, and a read-only look inside.
          Columns that hold credentials are listed but their values are masked.
        </p>
      </div>
      {inventoryError && <p className={styles.error}>{inventoryError}</p>}

      <div className={styles.columns}>
        <section className={styles.column} aria-label="Services">
          <h3 className={styles.heading}>Services{inventory ? ` (${services.length})` : ''}</h3>
          {!inventory && !inventoryError && <div className={styles.empty}>Reading services…</div>}
          {inventory && services.length === 0 && <div className={styles.empty}>healthcheck watches no services.</div>}
          {services.length > 0 && (
            <ul className={styles.serviceList}>
              {services.map(s => {
                const isSelected = s.name === selectedName
                return (
                  <li key={s.name}>
                    <button
                      type="button"
                      className={`${styles.serviceRow} ${isSelected ? styles.serviceRowSelected : ''}`}
                      aria-pressed={isSelected}
                      onClick={() => { setSelectedName(s.name); setSelectedPath(null) }}
                    >
                      <span className={styles.serviceHead}>
                        <span className={`${styles.dot} ${statusDotClass(s.status)}`} title={s.status} />
                        <span className={styles.serviceName}>{s.name}</span>
                        <span className={styles.chip}>{s.type}</span>
                      </span>
                      <span className={styles.serviceMeta}>
                        <span>{s.status}</span>
                        {s.pids.length > 0 && <span>pid {s.pids.join(', ')}</span>}
                        {s.databases.length > 0 && <span>{s.databases.length} db</span>}
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          )}
        </section>

        <section className={styles.column} aria-label="Service detail">
          {!selected && <div className={styles.empty}>Select a service to see its databases.</div>}
          {selected && (
            <ServiceDetail
              service={selected}
              selectedPath={selectedDatabase?.path ?? null}
              onSelectDatabase={setSelectedPath}
            />
          )}
          {selected && selectedDatabase && (
            <DatabasePanel key={selectedDatabase.path} fetchFn={fetchFn} basePath={basePath} database={selectedDatabase} />
          )}
        </section>
      </div>
    </div>
  )
}

function ServiceDetail({ service, selectedPath, onSelectDatabase }: {
  service: ServiceInventoryEntry
  selectedPath: string | null
  onSelectDatabase: (path: string) => void
}) {
  const identity = service.type === 'systemd'
    ? `${service.unit}.service (${service.system_unit ? 'system' : 'user'} unit)`
    : service.type === 'http' ? service.url : 'command check'
  return (
    <div className={styles.detail}>
      <h3 className={styles.heading}>
        <span className={`${styles.dot} ${statusDotClass(service.status)}`} />
        {service.name}
      </h3>
      <div className={styles.facts}>
        <span className={styles.factLabel}>Check</span><span className={styles.factValue}>{identity}</span>
        <span className={styles.factLabel}>Status</span>
        <span className={styles.factValue}>
          {service.status}{service.enabled_state ? ` · ${service.enabled_state}` : ''} · {service.response_ms} ms · last check {timeAgo(service.last_check)}
          {service.uptime_pct_24h !== undefined && ` · ${service.uptime_pct_24h.toFixed(1)}% of 24h`}
        </span>
        <span className={styles.factLabel}>Processes</span>
        <span className={styles.factValue}>
          {service.pids.length > 0 ? service.pids.join(', ') : service.type === 'command' ? 'none — a guard script, not a daemon' : 'none found'}
        </span>
        {service.process_lookup_error && (
          <><span className={styles.factLabel}>Lookup</span><span className={styles.lastError}>{service.process_lookup_error}</span></>
        )}
        {service.last_error && (
          <><span className={styles.factLabel}>Last error</span><span className={styles.lastError}>{service.last_error}</span></>
        )}
      </div>

      <h4 className={styles.heading}>Databases{` (${service.databases.length})`}</h4>
      {service.databases.length === 0 && (
        <div className={styles.empty}>
          {service.pids.length === 0 ? 'No process, so no open files to list.' : 'This process holds no SQLite file open right now.'}
        </div>
      )}
      {service.databases.length > 0 && (
        <ul className={styles.databaseList}>
          {service.databases.map(d => <DatabaseRow key={d.path} database={d} selected={d.path === selectedPath} onSelect={() => onSelectDatabase(d.path)} />)}
        </ul>
      )}
    </div>
  )
}

function DatabaseRow({ database, selected, onSelect }: { database: ServiceDatabase; selected: boolean; onSelect: () => void }) {
  return (
    <li>
      <button
        type="button"
        className={`${styles.databaseRow} ${selected ? styles.databaseRowSelected : ''}`}
        aria-pressed={selected}
        onClick={onSelect}
      >
        <span className={styles.databasePath} title={database.path}>{database.path}</span>
        <span className={styles.muted}>{formatBytes(database.size_bytes)}</span>
        {database.modified_at && <span className={styles.muted}>written {timeAgo(database.modified_at)}</span>}
        {database.shared_with.length > 0 && (
          <span className={`${styles.chip} ${styles.chipInfo}`} title={`also open in ${database.shared_with.join(', ')}`}>
            shared with {database.shared_with.join(', ')}
          </span>
        )}
      </button>
    </li>
  )
}

/** One database: its schema, and a rows panel under the opened table. Keyed
 *  on the path by the parent, so switching files resets every state below. */
function DatabasePanel({ fetchFn, basePath, database }: { fetchFn: FetchFn; basePath: string; database: ServiceDatabase }) {
  const [schema, setSchema] = useState<DatabaseSchemaResponse | null>(null)
  const [schemaError, setSchemaError] = useState<string | null>(null)
  const [openTable, setOpenTable] = useState<string | null>(null)

  useEffect(() => {
    let current = true
    readDatabaseSchema(fetchFn, basePath, database.path).then(result => {
      if (!current) return
      if (result.ok) { setSchema(result.value); setSchemaError(null) } else setSchemaError(result.error)
    })
    return () => { current = false }
  }, [fetchFn, basePath, database.path])

  return (
    <div className={styles.detail}>
      <h4 className={styles.heading}>
        Schema
        {schema && <span className={styles.muted}>{schema.tables.length} tables · {schema.tables.reduce((n, t) => n + t.row_count, 0)} rows</span>}
      </h4>
      {schemaError && <p className={styles.error}>{schemaError}</p>}
      {!schema && !schemaError && <div className={styles.empty}>Reading schema…</div>}
      {schema && schema.tables.length === 0 && <div className={styles.empty}>This file has no tables.</div>}
      {schema && schema.tables.length > 0 && (
        <ul className={styles.tableList}>
          {schema.tables.map(t => (
            <li key={t.name} className={styles.tableRow}>
              <button
                type="button"
                className={styles.tableHead}
                aria-expanded={openTable === t.name}
                onClick={() => setOpenTable(openTable === t.name ? null : t.name)}
              >
                <span className={styles.tableName}>{t.name}</span>
                <span className={styles.chip}>{t.kind}</span>
                <span className={styles.muted}>
                  {t.row_count_error ? `count failed: ${t.row_count_error}` : `${t.row_count} rows`} · {t.columns.length} columns
                </span>
              </button>
              {openTable === t.name && (
                <div className={styles.tableBody}>
                  <TableSchema table={t} />
                  <RowsPanel fetchFn={fetchFn} basePath={basePath} path={database.path} table={t} />
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function TableSchema({ table }: { table: DatabaseTable }) {
  return (
    <>
      <table className={styles.columnsTable}>
        <thead>
          <tr><th>column</th><th>type</th><th>constraints</th></tr>
        </thead>
        <tbody>
          {table.columns.map(c => (
            <tr key={c.name}>
              <td>{c.name}</td>
              <td>{c.type || <span className={styles.cellNull}>any</span>}</td>
              <td>
                {[c.primary_key && 'primary key', c.not_null && 'not null'].filter(Boolean).join(', ')}
                {c.masked && <span className={`${styles.chip} ${styles.chipWarn}`}> masked</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {table.sql && <pre className={styles.ddl}>{table.sql}</pre>}
    </>
  )
}

interface DraftFilter { column: string; op: DatabaseRowFilter['op']; value: string }

function RowsPanel({ fetchFn, basePath, path, table }: { fetchFn: FetchFn; basePath: string; path: string; table: DatabaseTable }) {
  const [limit, setLimit] = useState(50)
  const [orderBy, setOrderBy] = useState('')
  const [descending, setDescending] = useState(true)
  const [filters, setFilters] = useState<DraftFilter[]>([])
  const [draft, setDraft] = useState<DraftFilter>({ column: table.columns[0]?.name ?? '', op: 'eq', value: '' })
  const [rows, setRows] = useState<DatabaseRowsResponse | null>(null)
  const [rowsError, setRowsError] = useState<string | null>(null)
  const ticket = useRef(0)

  const filtersKey = JSON.stringify(filters)
  useEffect(() => {
    const mine = ++ticket.current
    setRowsError(null)
    readDatabaseRows(fetchFn, basePath, { path, table: table.name, limit, orderBy, descending, filters: JSON.parse(filtersKey) as DatabaseRowFilter[] })
      .then(result => {
        if (mine !== ticket.current) return
        if (result.ok) { setRows(result.value); setRowsError(null) } else setRowsError(result.error)
      })
  }, [fetchFn, basePath, path, table.name, limit, orderBy, descending, filtersKey])

  const addFilter = () => {
    if (!draft.column) return
    setFilters(f => [...f, draft])
    setDraft(d => ({ ...d, value: '' }))
  }
  const valueless = draft.op === 'null' || draft.op === 'not_null'

  return (
    <div className={styles.detail}>
      <div className={styles.rowsControls}>
        <label className={styles.muted}>
          order by{' '}
          <select className={styles.select} value={orderBy} onChange={e => setOrderBy(e.target.value)}>
            <option value="">{table.kind === 'table' ? 'rowid (insertion)' : 'storage order'}</option>
            {table.columns.map(c => <option key={c.name} value={c.name}>{c.name}</option>)}
          </select>
        </label>
        <button type="button" className={styles.button} onClick={() => setDescending(d => !d)} title="flip sort direction">
          {descending ? 'newest first' : 'oldest first'}
        </button>
        <label className={styles.muted}>
          limit{' '}
          <select className={styles.select} value={limit} onChange={e => setLimit(Number(e.target.value))}>
            {ROW_LIMITS.map(n => <option key={n} value={n}>{n}</option>)}
          </select>
        </label>
        {rows && <span className={styles.muted}>{rows.total_rows} match · showing {rows.rows.length}</span>}
      </div>

      <div className={styles.filterRow}>
        <select className={styles.select} value={draft.column} onChange={e => setDraft(d => ({ ...d, column: e.target.value }))} aria-label="Filter column">
          {table.columns.map(c => <option key={c.name} value={c.name}>{c.name}</option>)}
        </select>
        <select className={styles.select} value={draft.op} onChange={e => setDraft(d => ({ ...d, op: e.target.value as DatabaseRowFilter['op'] }))} aria-label="Filter operator">
          {FILTER_OPS.map(op => <option key={op} value={op}>{op}</option>)}
        </select>
        {!valueless && (
          <input
            className={styles.input}
            value={draft.value}
            placeholder="value"
            aria-label="Filter value"
            onChange={e => setDraft(d => ({ ...d, value: e.target.value }))}
            onKeyDown={e => { if (e.key === 'Enter') addFilter() }}
          />
        )}
        <button type="button" className={styles.button} onClick={addFilter}>Add filter</button>
        {filters.map((f, i) => (
          <button
            key={`${f.column}:${f.op}:${f.value}:${i}`}
            type="button"
            className={`${styles.chip} ${styles.chipInfo}`}
            title="remove this filter"
            onClick={() => setFilters(fs => fs.filter((_, j) => j !== i))}
          >
            {f.column} {f.op}{f.op === 'null' || f.op === 'not_null' ? '' : ` ${f.value}`} ✕
          </button>
        ))}
      </div>

      {rowsError && <p className={styles.error}>{rowsError}</p>}
      {!rows && !rowsError && <div className={styles.empty}>Reading rows…</div>}
      {rows && rows.rows.length === 0 && <div className={styles.empty}>No rows match.</div>}
      {rows && rows.rows.length > 0 && <RowsTable columns={rows.columns} rows={rows.rows} />}
    </div>
  )
}

function RowsTable({ columns, rows }: { columns: DatabaseColumn[]; rows: unknown[][] }) {
  return (
    <div className={styles.rowsWrap}>
      <table className={styles.rowsTable}>
        <thead>
          <tr>{columns.map(c => <th key={c.name}>{c.name}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i}>
              {columns.map((c, j) => <Cell key={c.name} column={c} value={row[j]} />)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** One value. A masked column says so rather than showing an empty cell, and
 *  a long value is shown whole only when clicked — the server sends it whole;
 *  the cut is this page's, so it is undone here too. */
function Cell({ column, value }: { column: DatabaseColumn; value: unknown }) {
  const [expanded, setExpanded] = useState(false)
  if (column.masked) return <td><span className={styles.cellMasked}>masked</span></td>
  if (value === null || value === undefined) return <td><span className={styles.cellNull}>null</span></td>
  const text = typeof value === 'string' ? value : JSON.stringify(value)
  if (text.length <= 80 && !text.includes('\n')) return <td title={text}>{text}</td>
  return (
    <td className={expanded ? styles.cellExpanded : undefined}>
      <button
        type="button"
        className={`${styles.cellButton} ${expanded ? styles.cellButtonExpanded : ''}`}
        title={expanded ? 'collapse' : `${text.length} characters — click to show all`}
        onClick={() => setExpanded(e => !e)}
      >
        {text}
      </button>
    </td>
  )
}
