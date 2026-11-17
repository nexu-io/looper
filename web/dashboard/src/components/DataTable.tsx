import type { CSSProperties, KeyboardEvent, ReactNode } from "react";

export type Column<T> = {
  key: string;
  header: string;
  className?: string;
  /**
   * Fixed column width (CSS length). Used with table-layout:fixed so content
   * changes don't reflow neighboring columns.
   */
  width?: string;
  /** When true, clicks inside this cell do not trigger onRowClick. */
  stopRowClick?: boolean;
  cell: (row: T) => ReactNode;
};

function colStyle(width?: string): CSSProperties | undefined {
  if (!width) return undefined;
  return { width, minWidth: width, maxWidth: width };
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  empty,
  onRowClick,
}: {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  empty?: ReactNode;
  onRowClick?: (row: T) => void;
}) {
  if (rows.length === 0) {
    return (
      <div className="py-4 text-center text-[12px] text-[var(--text-muted)]">
        {empty ?? "No items"}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full table-fixed border-collapse text-left text-[12px]">
        <colgroup>
          {columns.map((col) => (
            <col key={col.key} style={colStyle(col.width)} />
          ))}
        </colgroup>
        <thead>
          <tr className="border-b border-[var(--border)] text-[11px] uppercase tracking-wide text-[var(--text-muted)]">
            {columns.map((col) => (
              <th
                key={col.key}
                className={`px-2 py-1.5 align-middle font-medium ${col.className ?? ""}`}
                style={colStyle(col.width)}
              >
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr
              key={rowKey(row)}
              className={[
                "border-b border-[var(--border)] last:border-0",
                onRowClick
                  ? "cursor-pointer hover:bg-[var(--bg-muted)]"
                  : "",
              ].join(" ")}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
              onKeyDown={
                onRowClick
                  ? (e: KeyboardEvent<HTMLTableRowElement>) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        onRowClick(row);
                      }
                    }
                  : undefined
              }
              tabIndex={onRowClick ? 0 : undefined}
              role={onRowClick ? "link" : undefined}
            >
              {columns.map((col) => (
                <td
                  key={col.key}
                  className={`overflow-hidden px-2 py-1.5 align-middle ${col.className ?? ""}`}
                  style={colStyle(col.width)}
                  onClick={
                    col.stopRowClick
                      ? (e) => {
                          e.stopPropagation();
                        }
                      : undefined
                  }
                  onKeyDown={
                    col.stopRowClick
                      ? (e) => {
                          e.stopPropagation();
                        }
                      : undefined
                  }
                >
                  <div className="flex min-h-7 min-w-0 items-center">
                    {col.cell(row)}
                  </div>
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
