/**
 * Removes every trailing "/" from a string.
 *
 * A loop rather than `s.replace(/\/+$/, "")`: that pattern backtracks from
 * every slash in a run that is not at the end of the string, so its cost is
 * quadratic in the length of the run.
 */
export function stripTrailingSlashes(s: string): string {
  let end = s.length;
  while (end > 0 && s.charCodeAt(end - 1) === 47 /* "/" */) {
    end--;
  }
  return end === s.length ? s : s.slice(0, end);
}
