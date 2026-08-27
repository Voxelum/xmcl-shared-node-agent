export const PRODUCTION_OUTPUT_LIMITS = Object.freeze({
  maxFiles: 16_384,
  maxFileBytes: 256 * 1024 * 1024,
  maxSandboxBytes: 320 * 1024 * 1024,
  maxPackageInputBytes: 384 * 1024 * 1024,
  maxBundleArchiveBytes: 400 * 1024 * 1024,
  maxPackageTarBytes: 400 * 1024 * 1024,
  maxPackageArchiveBytes: 401 * 1024 * 1024,
});

export function outputSizeWithinLimit(sizes, maximum) {
  if (!Number.isSafeInteger(maximum) || maximum < 1) return false;
  let total = 0;
  for (const size of sizes) {
    if (!Number.isSafeInteger(size) || size < 0 ||
      size > PRODUCTION_OUTPUT_LIMITS.maxFileBytes) return false;
    total += size;
    if (!Number.isSafeInteger(total) || total > maximum) return false;
  }
  return true;
}
