// startPolling runs tick immediately and then again intervalMs after each
// tick settles, until the returned stop function is called. Chaining
// timeouts (rather than setInterval) means a slow response never has a
// second request stacked on top of it. Polling pauses while the tab is
// hidden and resumes with an immediate tick once it's visible again, so a
// dashboard left open in a background tab stops hitting the API. The signal
// passed to tick is aborted on stop, so an in-flight request can't update a
// component that has already unmounted.
export function startPolling(
  tick: (signal: AbortSignal) => Promise<unknown>,
  intervalMs: number,
): () => void {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let running = false;
  let stopped = false;

  const hidden = () => document.visibilityState === "hidden";

  const run = async () => {
    clearTimeout(timer);
    timer = undefined;
    if (stopped || running || hidden()) return;

    running = true;
    try {
      await tick(controller.signal);
    } catch {
      /* tick reports its own errors; a rejection must not stop the loop. */
    } finally {
      running = false;
    }

    if (!stopped && !hidden()) timer = setTimeout(() => void run(), intervalMs);
  };

  const onVisibilityChange = () => {
    if (!hidden() && timer === undefined && !running) void run();
  };

  document.addEventListener("visibilitychange", onVisibilityChange);
  void run();

  return () => {
    stopped = true;
    clearTimeout(timer);
    controller.abort();
    document.removeEventListener("visibilitychange", onVisibilityChange);
  };
}
