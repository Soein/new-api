/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type {
  MarketplaceIndex,
  MarketplaceIndexVersion,
  MarketplacePlugin,
  TaskPluginListItem,
} from '../types'
import { PluginSourceFetchError, readBoundedResponseText } from './plugin-url'

export const SUPPORTED_INDEX_VERSION = 1

/** The gateway only runs task plugins today; other kinds are filtered out. */
export const SUPPORTED_PLUGIN_KIND = 'task'

export const MAX_MARKETPLACE_INDEX_BYTES = 2 * 1024 * 1024
export const MAX_INDEX_PLUGINS = 500
export const MAX_PLUGIN_VERSIONS = 100
export const MAX_PLUGIN_KEY_LENGTH = 128
export const MAX_PLUGIN_NAME_LENGTH = 256
export const MAX_PLUGIN_ICON_LENGTH = 128
export const MAX_PLUGIN_DESC_LENGTH = 4096
export const MAX_PLUGIN_MODELS = 100
export const MAX_PLUGIN_MODEL_LENGTH = 128
export const MAX_PLUGIN_VERSION_LENGTH = 64
export const MAX_PLUGIN_PATH_LENGTH = 1024
export const MAX_PLUGIN_SHA256_LENGTH = 128
export const MAX_PLUGIN_AUTH_LENGTH = 128
export const MAX_PLUGIN_ALLOWED_HOSTS = 100
export const MAX_PLUGIN_HOST_LENGTH = 256
export const MAX_PLUGIN_CHANNEL_TYPES = 50

export type MarketplaceIndexFetchFailure =
  | 'unreachable'
  | 'not_found'
  | 'too_large'
  | 'invalid_json'
  | 'invalid_structure'
  | 'unsupported_version'
  | 'oversized_catalog'

export class MarketplaceIndexFetchError extends Error {
  constructor(
    public reason: MarketplaceIndexFetchFailure,
    public status?: number,
    message?: string
  ) {
    super(message || reason)
    this.name = 'MarketplaceIndexFetchError'
  }
}

export function formatMarketplaceError(
  error: unknown,
  t: (key: string, options?: Record<string, unknown>) => string
): string {
  if (error instanceof MarketplaceIndexFetchError) {
    switch (error.reason) {
      case 'unreachable':
        return t('Network error or host unreachable')
      case 'not_found':
        return error.status
          ? t('Index request failed with HTTP {{status}}', {
              status: error.status,
            })
          : t('Resource not found')
      case 'too_large':
        return t('Index payload exceeds size limit (2 MiB)')
      case 'invalid_json':
        return t('Invalid JSON in index response')
      case 'invalid_structure':
        return t('Index payload structure is invalid')
      case 'unsupported_version':
        return t('Unsupported index version')
      case 'oversized_catalog':
        return t('Index contains too many plugins (exceeds 500)')
      default:
        return error.message
    }
  }
  if (error instanceof Error) {
    return error.message
  }
  return String(error)
}

/**
 * Resolves a version's `path` against the index URL it was declared in, so the
 * same index works behind any raw prefix (GitHub raw, jsDelivr, a mirror).
 * Returns `null` when the path escapes to another origin or cannot be resolved.
 */
export function resolvePluginSourceUrl(
  indexUrl: string,
  path: string
): string | null {
  const trimmed = path.trim()
  if (!trimmed) return null
  let base: URL
  try {
    base = new URL(indexUrl)
  } catch {
    return null
  }
  let resolved: URL
  try {
    resolved = new URL(trimmed, base)
  } catch {
    return null
  }
  if (resolved.protocol !== 'http:' && resolved.protocol !== 'https:') {
    return null
  }
  // A relative path in an index must stay on the host that served the index;
  // an index that redirects source downloads elsewhere is not a source we can
  // reason about for integrity.
  if (resolved.origin !== base.origin) return null
  return resolved.toString()
}

/**
 * Validates an untrusted index payload into the display shape. Unknown fields
 * are dropped and malformed plugin entries are skipped rather than failing the
 * whole source, because the index is only a display cache — admission still
 * happens server-side on the compiled source.
 */
export function parseMarketplaceIndex(payload: unknown): MarketplaceIndex {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) {
    throw new MarketplaceIndexFetchError(
      'invalid_structure',
      undefined,
      'index is not an object'
    )
  }
  const raw = payload as Record<string, unknown>
  const indexVersion = Number(raw.indexVersion)
  if (!Number.isFinite(indexVersion)) {
    throw new MarketplaceIndexFetchError(
      'invalid_structure',
      undefined,
      'index is missing indexVersion'
    )
  }
  if (indexVersion > SUPPORTED_INDEX_VERSION || indexVersion < 1) {
    throw new MarketplaceIndexFetchError(
      'unsupported_version',
      undefined,
      `unsupported indexVersion ${indexVersion}`
    )
  }
  if (Array.isArray(raw.plugins) && raw.plugins.length > MAX_INDEX_PLUGINS) {
    throw new MarketplaceIndexFetchError(
      'oversized_catalog',
      undefined,
      `index exceeds maximum plugin limit of ${MAX_INDEX_PLUGINS}`
    )
  }
  const plugins: MarketplacePlugin[] = []
  if (Array.isArray(raw.plugins)) {
    const seenKeys = new Set<string>()
    for (const entry of raw.plugins) {
      const plugin = parseMarketplacePlugin(entry)
      if (plugin && !seenKeys.has(plugin.key)) {
        seenKeys.add(plugin.key)
        plugins.push(plugin)
      }
    }
  }
  return {
    indexVersion,
    name:
      typeof raw.name === 'string'
        ? raw.name.slice(0, MAX_PLUGIN_NAME_LENGTH)
        : '',
    plugins,
  }
}

function parseMarketplacePlugin(entry: unknown): MarketplacePlugin | null {
  if (!entry || typeof entry !== 'object' || Array.isArray(entry)) return null
  const raw = entry as Record<string, unknown>
  const rawKey = typeof raw.key === 'string' ? raw.key.trim() : ''
  if (!rawKey || rawKey.length > MAX_PLUGIN_KEY_LENGTH) return null
  const key = rawKey

  if (!Array.isArray(raw.versions)) return null
  if (raw.versions.length > MAX_PLUGIN_VERSIONS) return null

  const versions: MarketplaceIndexVersion[] = []
  for (const candidate of raw.versions) {
    if (
      !candidate ||
      typeof candidate !== 'object' ||
      Array.isArray(candidate)
    ) {
      continue
    }
    const rawVersion = candidate as Record<string, unknown>
    const rawV =
      typeof rawVersion.version === 'string' ? rawVersion.version.trim() : ''
    const rawP =
      typeof rawVersion.path === 'string' ? rawVersion.path.trim() : ''
    if (!rawV || !rawP) continue
    if (rawV.length > MAX_PLUGIN_VERSION_LENGTH) continue
    if (rawP.length > MAX_PLUGIN_PATH_LENGTH) continue

    if (versions.some((v) => v.version === rawV)) continue

    let sha256: string | undefined
    if (typeof rawVersion.sha256 === 'string') {
      const trimmedSha = rawVersion.sha256.trim()
      if (trimmedSha.length > MAX_PLUGIN_SHA256_LENGTH) continue
      if (trimmedSha) sha256 = trimmedSha
    }

    let auth: string | undefined
    if (typeof rawVersion.auth === 'string') {
      const trimmedAuth = rawVersion.auth.trim()
      if (trimmedAuth.length > MAX_PLUGIN_AUTH_LENGTH) continue
      if (trimmedAuth) auth = trimmedAuth
    }

    let allowedHosts: string[] | undefined
    if (Array.isArray(rawVersion.allowedHosts)) {
      if (rawVersion.allowedHosts.length > MAX_PLUGIN_ALLOWED_HOSTS) continue
      const hosts: string[] = []
      let invalidHost = false
      for (const host of rawVersion.allowedHosts) {
        if (typeof host !== 'string' || host.length > MAX_PLUGIN_HOST_LENGTH) {
          invalidHost = true
          break
        }
        hosts.push(host)
      }
      if (invalidHost) continue
      if (hosts.length > 0) allowedHosts = hosts
    }

    const kind =
      typeof rawVersion.kind === 'string' ? rawVersion.kind.trim() : ''
    if (kind && kind !== SUPPORTED_PLUGIN_KIND) continue

    versions.push({
      version: rawV,
      path: rawP,
      sha256,
      minApiVersion: Number.isFinite(Number(rawVersion.minApiVersion))
        ? Number(rawVersion.minApiVersion)
        : undefined,
      kind: kind || undefined,
      allowedHosts,
      auth,
    })
  }

  if (versions.length === 0) return null

  const declaredLatest = typeof raw.latest === 'string' ? raw.latest.trim() : ''
  const latest = versions.some((entry) => entry.version === declaredLatest)
    ? declaredLatest
    : versions[0].version

  let icon: string | undefined
  if (typeof raw.icon === 'string') {
    const trimmed = raw.icon.trim()
    if (trimmed && trimmed.length <= MAX_PLUGIN_ICON_LENGTH) {
      icon = trimmed
    }
  }

  const rawName =
    typeof raw.name === 'string' && raw.name.trim() ? raw.name.trim() : key
  const name = rawName.slice(0, MAX_PLUGIN_NAME_LENGTH)

  return {
    key,
    name,
    icon,
    description: parseMarketplaceDescription(raw.description),
    channelTypes: parsePluginChannelTypes(raw.channelTypes),
    models: parsePluginModels(raw.models),
    latest,
    versions,
  }
}

function parseMarketplaceDescription(
  value: unknown
): string | Record<string, string> | undefined {
  if (typeof value === 'string') return value.slice(0, MAX_PLUGIN_DESC_LENGTH)
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return undefined
  }
  const mapped: Record<string, string> = {}
  for (const [locale, text] of Object.entries(
    value as Record<string, unknown>
  ).slice(0, 20)) {
    if (typeof text === 'string') {
      mapped[locale.slice(0, 32)] = text.slice(0, MAX_PLUGIN_DESC_LENGTH)
    }
  }
  return Object.keys(mapped).length > 0 ? mapped : undefined
}

function parsePluginModels(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined
  if (value.length > MAX_PLUGIN_MODELS) return undefined
  const models: string[] = []
  for (const item of value) {
    if (typeof item === 'string' && item.length <= MAX_PLUGIN_MODEL_LENGTH) {
      models.push(item)
    }
  }
  return models.length > 0 ? models : undefined
}

function parsePluginChannelTypes(value: unknown): number[] | undefined {
  if (!Array.isArray(value)) return undefined
  if (value.length > MAX_PLUGIN_CHANNEL_TYPES) return undefined
  const items = value.filter(
    (item): item is number => typeof item === 'number' && Number.isFinite(item)
  )
  return items.length > 0 ? items : undefined
}

export async function fetchMarketplaceIndex(
  url: string,
  fetchImpl: typeof fetch = globalThis.fetch
): Promise<MarketplaceIndex> {
  let response: Response
  try {
    response = await fetchImpl(url)
  } catch {
    throw new MarketplaceIndexFetchError('unreachable')
  }
  if (!response.ok) {
    throw new MarketplaceIndexFetchError('not_found', response.status)
  }
  let text: string
  try {
    text = await readBoundedResponseText(response, MAX_MARKETPLACE_INDEX_BYTES)
  } catch (err) {
    if (err instanceof PluginSourceFetchError && err.reason === 'too_large') {
      throw new MarketplaceIndexFetchError('too_large')
    }
    if (err instanceof MarketplaceIndexFetchError) {
      throw err
    }
    throw new MarketplaceIndexFetchError('unreachable')
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    throw new MarketplaceIndexFetchError('invalid_json')
  }
  return parseMarketplaceIndex(parsed)
}

export function findMarketplaceVersion(
  plugin: MarketplacePlugin,
  version: string
): MarketplaceIndexVersion | undefined {
  return plugin.versions.find((entry) => entry.version === version)
}

export type InstallState =
  | { status: 'not_installed' }
  | { status: 'up_to_date'; installedVersion: string }
  | { status: 'upgradable'; installedVersion: string; latestVersion: string }
  | { status: 'diverged'; installedVersion: string; latestVersion: string }

/**
 * Compares a marketplace entry against the gateway's installed plugins.
 *
 * `diverged` covers the case where a plugin is installed at a version the index
 * does not list (locally uploaded, or the source rolled a version back): the UI
 * must not call that an upgrade, because installing would move the gateway to a
 * version it may already have moved away from deliberately.
 */
export function deriveInstallState(
  plugin: MarketplacePlugin,
  installed: TaskPluginListItem[]
): InstallState {
  const match = installed.find((item) => item.meta.key === plugin.key)
  if (!match) return { status: 'not_installed' }

  const installedVersion = match.meta.version
  if (installedVersion === plugin.latest) {
    return { status: 'up_to_date', installedVersion }
  }
  const known = plugin.versions.some(
    (entry) => entry.version === installedVersion
  )
  if (!known) {
    return {
      status: 'diverged',
      installedVersion,
      latestVersion: plugin.latest,
    }
  }
  return {
    status: 'upgradable',
    installedVersion,
    latestVersion: plugin.latest,
  }
}

export type MarketplaceActionPolicy =
  | { kind: 'install' }
  | { kind: 'system_update' }

/**
 * Factory-served plugins are compiled into the binary and must only update
 * with a system release. Marketplace install would create a permanent override
 * that shadows every future built-in update — that action is suppressed.
 * Overrides and third-party plugins still install/upgrade normally.
 */
export function resolveMarketplaceActionPolicy(
  installed?: TaskPluginListItem
): MarketplaceActionPolicy {
  if (installed?.source === 'factory') {
    return { kind: 'system_update' }
  }
  return { kind: 'install' }
}

/**
 * Built-in version shown next to the marketplace latest. Factory-served items
 * do not carry `factory_meta` (their `meta` *is* the factory meta); overridden
 * factory plugins expose the shadowed built-in on `factory_meta`.
 */
export function marketplaceBuiltInVersion(
  installed?: TaskPluginListItem
): string | undefined {
  if (!installed) return undefined
  if (installed.source === 'factory') return installed.meta.version
  return installed.factory_meta?.version
}

export function isStaleFactoryOverride(item: TaskPluginListItem): boolean {
  return (
    item.source === 'override_over_factory' &&
    item.factory_meta != null &&
    item.factory_meta.version !== item.meta.version
  )
}

/**
 * A source is only integrity-checked when every listed version carries a
 * sha256. Anything less and installs from it cannot be pinned, so the UI warns.
 */
export function indexHasIntegrityHashes(index: MarketplaceIndex): boolean {
  return (
    index.plugins.length > 0 &&
    index.plugins.every((plugin) =>
      plugin.versions.every((version) => Boolean(version.sha256))
    )
  )
}

export const DEFAULT_MARKETPLACE_INDEX_URL =
  'https://www.newapi.ai/api/v1/plugins/index.json'

export const GITHUB_MARKETPLACE_INDEX_URL =
  'https://raw.githubusercontent.com/QuantumNous/new-api-plugins/main/index.json'

/**
 * Both built-in indexes are maintained by the project. Other configured
 * sources get an explicit at-your-own-risk label.
 */
export function isDefaultMarketplaceSource(indexUrl: string): boolean {
  const normalized = indexUrl.trim()
  return (
    normalized === DEFAULT_MARKETPLACE_INDEX_URL ||
    normalized === GITHUB_MARKETPLACE_INDEX_URL
  )
}
