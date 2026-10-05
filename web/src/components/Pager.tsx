"use client";

// S-239: shared list pager. Page navigation plus an items-per-page
// selector (default 25), styled with the platform's existing controls
// (btn btn-small + form-control) so it reads like the rest of the UI.
// Purely presentational: the owning page keeps page/size state and
// re-fetches on change.

export const PAGE_SIZE_CHOICES = [10, 25, 50, 100] as const;
export const DEFAULT_PAGE_SIZE = 25;

export type PagerProps = {
  readonly page: number; // 1-based
  readonly pageSize: number;
  readonly total: number;
  readonly onPageChange: (page: number) => void;
  readonly onPageSizeChange: (pageSize: number) => void;
};

export function pageCount(total: number, pageSize: number): number {
  return Math.max(1, Math.ceil(total / Math.max(1, pageSize)));
}

export function Pager({ page, pageSize, total, onPageChange, onPageSizeChange }: PagerProps) {
  const pages = pageCount(total, pageSize);
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const to = Math.min(total, page * pageSize);
  return (
    <nav className="pager" aria-label="Pagination">
      <span className="pager-summary">
        Showing {from}–{to} of {total}
      </span>
      <div className="pager-controls">
        <label className="pager-size">
          <span>Items per page</span>
          <select
            className="form-control"
            value={pageSize}
            aria-label="Items per page"
            onChange={(e) => onPageSizeChange(Number(e.target.value))}
          >
            {PAGE_SIZE_CHOICES.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <button
          type="button"
          className="btn btn-small pager-prev"
          disabled={page <= 1}
          aria-label="Previous page"
          onClick={() => onPageChange(page - 1)}
        >
          ← Previous
        </button>
        <span className="pager-page">
          Page {page} of {pages}
        </span>
        <button
          type="button"
          className="btn btn-small pager-next"
          disabled={page >= pages}
          aria-label="Next page"
          onClick={() => onPageChange(page + 1)}
        >
          Next →
        </button>
      </div>
    </nav>
  );
}
