// Error reporting to Sentry, enabled by the backend's config.yaml sentry:
// entry (see GET /api/client-config). The Content-Security-Policy only lets
// the dashboard reach that DSN's host while it's set (see
// internal/backup/webui/headers.go).
//
// @sentry/react is only loaded (see initSentry) when a DSN is configured, so
// an install without Sentry never downloads it.
import type { Breadcrumb, ErrorEvent } from "@sentry/react";
import { fetchClientConfig } from "../api/client";

/**
 * Starts reporting the dashboard's uncaught errors and unhandled promise
 * rejections to Sentry, if the backend has a DSN for it. Errors raised before
 * the SDK finishes loading aren't reported. Never throws: a missing error
 * report must not break the dashboard.
 */
export async function initSentry() {
  try {
    const { sentry } = await fetchClientConfig();
    if (!sentry?.dsn) return;

    const Sentry = await import("@sentry/react");
    Sentry.init({
      dsn: sentry.dsn,
      environment: sentry.environment,
      release: sentry.release,
      beforeSend: scrubEvent,
      beforeBreadcrumb: scrubBreadcrumb,
    });
  } catch {
    // No config or no SDK: run without error reporting.
  }
}

/**
 * Drops a URL's query string and fragment: they may carry download and live
 * tickets or an OIDC callback's code and state.
 */
export function stripQuery(url: string): string {
  return url.replace(/[?#].*$/, "");
}

function scrubEvent(event: ErrorEvent): ErrorEvent {
  if (event.request) {
    event.request = {
      url: event.request.url && stripQuery(event.request.url),
      method: event.request.method,
    };
  }

  return event;
}

function scrubBreadcrumb(breadcrumb: Breadcrumb): Breadcrumb {
  const data = breadcrumb.data;
  if (!data) return breadcrumb;

  // fetch/xhr breadcrumbs carry url; navigation breadcrumbs from and to.
  for (const key of ["url", "from", "to"]) {
    if (typeof data[key] === "string") data[key] = stripQuery(data[key]);
  }

  return breadcrumb;
}
