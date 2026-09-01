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
import assert from 'node:assert/strict'

import { describe, test } from 'vitest'

import {
  deriveInstallState,
  fetchMarketplaceIndex,
  findMarketplaceVersion,
  formatMarketplaceError,
  GITHUB_MARKETPLACE_INDEX_URL,
  indexHasIntegrityHashes,
  isDefaultMarketplaceSource,
  isStaleFactoryOverride,
  marketplaceBuiltInVersion,
  MarketplaceIndexFetchError,
  MAX_INDEX_PLUGINS,
  MAX_MARKETPLACE_INDEX_BYTES,
  MAX_PLUGIN_ALLOWED_HOSTS,
  MAX_PLUGIN_AUTH_LENGTH,
  MAX_PLUGIN_HOST_LENGTH,
  MAX_PLUGIN_KEY_LENGTH,
  MAX_PLUGIN_PATH_LENGTH,
  MAX_PLUGIN_SHA256_LENGTH,
  MAX_PLUGIN_VERSION_LENGTH,
  MAX_PLUGIN_VERSIONS,
  parseMarketplaceIndex,
  resolveMarketplaceActionPolicy,
  resolvePluginSourceUrl,
} from '../lib/marketplace'
import type {
  MarketplaceIndex,
  MarketplacePlugin,
  TaskPluginListItem,
} from '../types'

const OFFICIAL_INDEX_URL = 'https://www.newapi.ai/api/v1/plugins/index.json'

function marketplacePlugin(
  overrides: Partial<MarketplacePlugin> = {}
): MarketplacePlugin {
  return {
    key: 'doubao',
    name: 'doubao-video',
    latest: '1.2.0',
    versions: [
      { version: '1.2.0', path: 'plugins/tasks/doubao/1.2.0/plugin.js' },
      { version: '1.0.0', path: 'plugins/tasks/doubao/1.0.0/plugin.js' },
    ],
    ...overrides,
  }
}

function factoryMeta(key: string, version: string) {
  return {
    apiVersion: 1,
    key,
    name: key,
    version,
    author: { name: 'test' as const },
    models: null,
    fetchMode: 'poll',
  }
}

function installedPlugin(
  key: string,
  version: string,
  overrides: Partial<TaskPluginListItem> = {}
): TaskPluginListItem {
  return {
    meta: {
      apiVersion: 1,
      key,
      name: key,
      version,
      author: { name: 'test' },
      models: null,
      fetchMode: 'poll',
    },
    source: 'override',
    enabled: true,
    active: true,
    source_hash: 'hash',
    remark: '',
    runtime_status: 'registered',
    channel_count: 0,
    in_flight_count: 0,
    ...overrides,
  }
}

describe('marketplace source path resolution', () => {
  test('resolves a relative path against the directory holding the index', () => {
    assert.equal(
      resolvePluginSourceUrl(
        'https://host.example/x/index.json',
        'plugins/tasks/doubao/1.0.0/plugin.js'
      ),
      'https://host.example/x/plugins/tasks/doubao/1.0.0/plugin.js'
    )
  })

  test('resolves against a root index without dropping the path', () => {
    assert.equal(
      resolvePluginSourceUrl(OFFICIAL_INDEX_URL, 'x/1.0.0/plugin.js'),
      'https://www.newapi.ai/api/v1/plugins/x/1.0.0/plugin.js'
    )
  })

  test('resolves a root-relative path against the index origin', () => {
    assert.equal(
      resolvePluginSourceUrl(
        'https://host.example/x/index.json',
        '/other/plugin.js'
      ),
      'https://host.example/other/plugin.js'
    )
  })

  test('rejects a path that resolves to a different origin', () => {
    assert.equal(
      resolvePluginSourceUrl(
        'https://host.example/x/index.json',
        'https://evil.example/plugin.js'
      ),
      null
    )
  })

  test('rejects an empty path', () => {
    assert.equal(resolvePluginSourceUrl(OFFICIAL_INDEX_URL, '  '), null)
  })

  test('rejects an index URL that is not a valid absolute URL', () => {
    assert.equal(resolvePluginSourceUrl('not-a-url', 'plugin.js'), null)
  })
})

describe('marketplace index parsing', () => {
  test('keeps plugins whose kind is absent or task', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      name: 'Official',
      plugins: [
        {
          key: 'no-kind',
          latest: '1.0.0',
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
        {
          key: 'task-kind',
          latest: '1.0.0',
          versions: [{ version: '1.0.0', path: 'b/plugin.js', kind: 'task' }],
        },
      ],
    })
    assert.deepEqual(
      index.plugins.map((plugin) => plugin.key),
      ['no-kind', 'task-kind']
    )
  })

  test('drops a plugin whose only version declares an unsupported kind', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'relay-only',
          latest: '1.0.0',
          versions: [{ version: '1.0.0', path: 'a/plugin.js', kind: 'relay' }],
        },
      ],
    })
    assert.deepEqual(index.plugins, [])
  })

  test('carries the optional allowedHosts and auth declarations through', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'doubao',
          latest: '1.0.0',
          versions: [
            {
              version: '1.0.0',
              path: 'a/plugin.js',
              sha256: 'abc',
              allowedHosts: ['ark.cn-beijing.volces.com'],
              auth: 'api_key',
            },
          ],
        },
      ],
    })
    assert.deepEqual(index.plugins[0].versions[0].allowedHosts, [
      'ark.cn-beijing.volces.com',
    ])
    assert.equal(index.plugins[0].versions[0].auth, 'api_key')
  })

  test('falls back to the first listed version when latest names an absent version', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'doubao',
          latest: '9.9.9',
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].latest, '1.0.0')
  })

  test('skips malformed plugin entries instead of failing the whole source', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        null,
        { name: 'no key' },
        { key: 'no-versions', versions: [] },
        {
          key: 'ok',
          latest: '1.0.0',
          versions: [{ version: '1.0.0', path: 'a.js' }],
        },
      ],
    })
    assert.deepEqual(
      index.plugins.map((plugin) => plugin.key),
      ['ok']
    )
  })

  test('rejects an index version newer than this gateway understands', () => {
    assert.throws(
      () => parseMarketplaceIndex({ indexVersion: 2, plugins: [] }),
      /unsupported indexVersion 2/
    )
  })

  test('rejects a payload with no indexVersion', () => {
    assert.throws(
      () => parseMarketplaceIndex({ plugins: [] }),
      /missing indexVersion/
    )
  })

  test('rejects a non-object payload', () => {
    assert.throws(() => parseMarketplaceIndex('<html>'), /not an object/)
  })

  test('keeps a present icon string after trim', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'sora',
          latest: '1.0.0',
          icon: '  Sora.Color  ',
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].icon, 'Sora.Color')
  })

  test('omits icon when the field is absent', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'sora',
          latest: '1.0.0',
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].icon, undefined)
  })

  test('drops a non-string icon', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'sora',
          latest: '1.0.0',
          icon: 12,
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].icon, undefined)
  })

  test('keeps a bare string description from a legacy index', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'kling',
          latest: '1.0.0',
          description: 'Video generation via Kling API',
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].description, 'Video generation via Kling API')
  })

  test('keeps a LocalizedText object description from a current index', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'kling',
          latest: '1.0.0',
          description: {
            en: 'Video generation via Kling API',
            zh: '可灵视频生成',
          },
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.deepEqual(index.plugins[0].description, {
      en: 'Video generation via Kling API',
      zh: '可灵视频生成',
    })
  })

  test('omits a non-string, non-object description', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'kling',
          latest: '1.0.0',
          description: 12,
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].description, undefined)
  })

  test('drops an icon longer than 128 characters', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'sora',
          latest: '1.0.0',
          icon: 'A'.repeat(129),
          versions: [{ version: '1.0.0', path: 'a/plugin.js' }],
        },
      ],
    })
    assert.equal(index.plugins[0].icon, undefined)
  })
})

describe('install state derivation', () => {
  test('reports not installed when no local plugin shares the key', () => {
    assert.deepEqual(deriveInstallState(marketplacePlugin(), []), {
      status: 'not_installed',
    })
  })

  test('reports up to date when the installed version equals latest', () => {
    assert.deepEqual(
      deriveInstallState(marketplacePlugin(), [
        installedPlugin('doubao', '1.2.0'),
      ]),
      { status: 'up_to_date', installedVersion: '1.2.0' }
    )
  })

  test('reports upgradable when an older listed version is installed', () => {
    assert.deepEqual(
      deriveInstallState(marketplacePlugin(), [
        installedPlugin('doubao', '1.0.0'),
      ]),
      {
        status: 'upgradable',
        installedVersion: '1.0.0',
        latestVersion: '1.2.0',
      }
    )
  })

  test('reports diverged when the installed version is absent from the index', () => {
    assert.deepEqual(
      deriveInstallState(marketplacePlugin(), [
        installedPlugin('doubao', '3.0.0-local'),
      ]),
      {
        status: 'diverged',
        installedVersion: '3.0.0-local',
        latestVersion: '1.2.0',
      }
    )
  })

  test('ignores installed plugins with a different key', () => {
    assert.deepEqual(
      deriveInstallState(marketplacePlugin(), [
        installedPlugin('kling', '1.2.0'),
      ]),
      { status: 'not_installed' }
    )
  })
})

describe('marketplace action policy', () => {
  test('factory-served plugin returns the informational system-update state', () => {
    assert.deepEqual(
      resolveMarketplaceActionPolicy(
        installedPlugin('doubao', '1.0.0', { source: 'factory' })
      ),
      { kind: 'system_update' }
    )
  })

  test('overridden factory plugin still allows marketplace install', () => {
    assert.deepEqual(
      resolveMarketplaceActionPolicy(
        installedPlugin('doubao', '1.2.0', {
          source: 'override_over_factory',
          factory_meta: factoryMeta('doubao', '1.0.0'),
        })
      ),
      { kind: 'install' }
    )
  })

  test('third-party plugin still allows marketplace install', () => {
    assert.deepEqual(
      resolveMarketplaceActionPolicy(installedPlugin('doubao', '1.0.0')),
      { kind: 'install' }
    )
  })

  test('uninstalled plugin still allows marketplace install', () => {
    assert.deepEqual(resolveMarketplaceActionPolicy(undefined), {
      kind: 'install',
    })
  })

  test('factory-served built-in version is the installed meta version', () => {
    assert.equal(
      marketplaceBuiltInVersion(
        installedPlugin('doubao', '1.0.0', { source: 'factory' })
      ),
      '1.0.0'
    )
  })

  test('overridden factory built-in version comes from factory_meta', () => {
    assert.equal(
      marketplaceBuiltInVersion(
        installedPlugin('doubao', '1.2.0', {
          source: 'override_over_factory',
          factory_meta: factoryMeta('doubao', '1.0.0'),
        })
      ),
      '1.0.0'
    )
  })
})

describe('stale factory override', () => {
  test('is stale when override version differs from built-in', () => {
    assert.equal(
      isStaleFactoryOverride(
        installedPlugin('doubao', '1.2.0', {
          source: 'override_over_factory',
          factory_meta: factoryMeta('doubao', '1.0.0'),
        })
      ),
      true
    )
  })

  test('is not stale when override version matches built-in', () => {
    assert.equal(
      isStaleFactoryOverride(
        installedPlugin('doubao', '1.0.0', {
          source: 'override_over_factory',
          factory_meta: factoryMeta('doubao', '1.0.0'),
        })
      ),
      false
    )
  })

  test('is not stale for factory-served or third-party plugins', () => {
    assert.equal(
      isStaleFactoryOverride(
        installedPlugin('doubao', '1.0.0', { source: 'factory' })
      ),
      false
    )
    assert.equal(
      isStaleFactoryOverride(installedPlugin('doubao', '1.0.0')),
      false
    )
  })
})

describe('marketplace version lookup', () => {
  test('finds the entry matching a version', () => {
    assert.equal(
      findMarketplaceVersion(marketplacePlugin(), '1.0.0')?.path,
      'plugins/tasks/doubao/1.0.0/plugin.js'
    )
  })

  test('returns undefined for an unknown version', () => {
    assert.equal(
      findMarketplaceVersion(marketplacePlugin(), '9.9.9'),
      undefined
    )
  })
})

describe('source integrity and trust labels', () => {
  function index(plugins: MarketplacePlugin[]): MarketplaceIndex {
    return { indexVersion: 1, name: 'test', plugins }
  }

  test('treats a source as verified only when every version carries a hash', () => {
    assert.equal(
      indexHasIntegrityHashes(
        index([
          marketplacePlugin({
            versions: [
              { version: '1.2.0', path: 'a.js', sha256: 'aa' },
              { version: '1.0.0', path: 'b.js', sha256: 'bb' },
            ],
          }),
        ])
      ),
      true
    )
  })

  test('treats a partially hashed source as unverified', () => {
    assert.equal(
      indexHasIntegrityHashes(
        index([
          marketplacePlugin({
            versions: [
              { version: '1.2.0', path: 'a.js', sha256: 'aa' },
              { version: '1.0.0', path: 'b.js' },
            ],
          }),
        ])
      ),
      false
    )
  })

  test('treats an empty index as unverified rather than trivially verified', () => {
    assert.equal(indexHasIntegrityHashes(index([])), false)
  })

  test('labels both built-in index URLs as official sources', () => {
    assert.equal(isDefaultMarketplaceSource(OFFICIAL_INDEX_URL), true)
    assert.equal(isDefaultMarketplaceSource(` ${OFFICIAL_INDEX_URL} `), true)
    assert.equal(isDefaultMarketplaceSource(GITHUB_MARKETPLACE_INDEX_URL), true)
  })

  test('labels any other index URL as third-party', () => {
    assert.equal(
      isDefaultMarketplaceSource('https://mirror.example/index.json'),
      false
    )
  })
})

describe('marketplace index boundaries, security, and error contracts', () => {
  test('rejects oversized plugin catalogs instead of silently slicing them', () => {
    const rawPlugins = Array.from(
      { length: MAX_INDEX_PLUGINS + 1 },
      (_, i) => ({
        key: `plugin-${i}`,
        name: `Plugin ${i}`,
        versions: [{ version: '1.0.0', path: `plugin-${i}.js` }],
      })
    )
    assert.throws(
      () =>
        parseMarketplaceIndex({
          indexVersion: 1,
          name: 'Huge catalog',
          plugins: rawPlugins,
        }),
      (err: unknown) => {
        assert.ok(err instanceof MarketplaceIndexFetchError)
        assert.equal(err.reason, 'oversized_catalog')
        return true
      }
    )
  })

  test('skips plugins with oversized versions list instead of silently slicing', () => {
    const rawVersions = Array.from(
      { length: MAX_PLUGIN_VERSIONS + 1 },
      (_, i) => ({
        version: `1.${i}.0`,
        path: `plugin-1.${i}.0.js`,
      })
    )
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'many-versions',
          versions: rawVersions,
        },
        {
          key: 'valid-plugin',
          versions: [{ version: '1.0.0', path: 'valid.js' }],
        },
      ],
    })
    // 'many-versions' is skipped because its versions list exceeds MAX_PLUGIN_VERSIONS
    assert.equal(index.plugins.length, 1)
    assert.equal(index.plugins[0].key, 'valid-plugin')
  })

  test('rejects oversized key without truncation to prevent identity collisions', () => {
    const prefix = 'k'.repeat(MAX_PLUGIN_KEY_LENGTH)
    const key1 = `${prefix}-1`
    const key2 = `${prefix}-2`
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: key1,
          name: 'Plugin 1',
          versions: [{ version: '1.0.0', path: 'p1.js' }],
        },
        {
          key: key2,
          name: 'Plugin 2',
          versions: [{ version: '1.0.0', path: 'p2.js' }],
        },
        {
          key: 'valid-key',
          name: 'Plugin Valid',
          versions: [{ version: '1.0.0', path: 'pv.js' }],
        },
      ],
    })
    // Both oversized keys are dropped; no collision occurs
    assert.equal(index.plugins.length, 1)
    assert.equal(index.plugins[0].key, 'valid-key')
  })

  test('skips versions with oversized security/identity fields while preserving valid versions', () => {
    const hugeVersion = 'v'.repeat(MAX_PLUGIN_VERSION_LENGTH + 1)
    const hugePath = 'p'.repeat(MAX_PLUGIN_PATH_LENGTH + 1)
    const hugeSha = 's'.repeat(MAX_PLUGIN_SHA256_LENGTH + 1)
    const hugeAuth = 'a'.repeat(MAX_PLUGIN_AUTH_LENGTH + 1)
    const hugeHost = 'h'.repeat(MAX_PLUGIN_HOST_LENGTH + 1)

    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'my-plugin',
          name: 'n'.repeat(500), // Display name is safely bounded
          description: 'd'.repeat(10000), // Display description is safely bounded
          versions: [
            { version: hugeVersion, path: 'p1.js' },
            { version: '1.0.0', path: hugePath },
            { version: '1.0.1', path: 'p3.js', sha256: hugeSha },
            { version: '1.0.2', path: 'p4.js', auth: hugeAuth },
            { version: '1.0.3', path: 'p5.js', allowedHosts: [hugeHost] },
            {
              version: '1.0.4',
              path: 'p6.js',
              allowedHosts: Array.from(
                { length: MAX_PLUGIN_ALLOWED_HOSTS + 1 },
                (_, i) => `host-${i}.com`
              ),
            },
            { version: '1.0.5', path: 'valid/path.js', sha256: 'abc123' },
          ],
        },
      ],
    })
    assert.equal(index.plugins.length, 1)
    const plugin = index.plugins[0]
    assert.equal(plugin.versions.length, 1)
    assert.equal(plugin.versions[0].version, '1.0.5')
    assert.equal(plugin.versions[0].path, 'valid/path.js')
    assert.equal(plugin.versions[0].sha256, 'abc123')
    assert.ok(plugin.name.length <= 256)
    assert.ok((plugin.description as string).length <= 4096)
  })

  test('deduplicates duplicate plugin keys and duplicate version entries', () => {
    const index = parseMarketplaceIndex({
      indexVersion: 1,
      plugins: [
        {
          key: 'doubao',
          name: 'Doubao First',
          versions: [
            { version: '1.0.0', path: 'p1.js' },
            { version: '1.0.0', path: 'p1-dup.js' },
          ],
        },
        {
          key: 'doubao',
          name: 'Doubao Duplicate',
          versions: [{ version: '2.0.0', path: 'p2.js' }],
        },
      ],
    })
    assert.equal(index.plugins.length, 1)
    assert.equal(index.plugins[0].name, 'Doubao First')
    assert.equal(index.plugins[0].versions.length, 1)
    assert.equal(index.plugins[0].versions[0].path, 'p1.js')
  })

  test('fetchMarketplaceIndex rejects oversized chunked index bodies', async () => {
    const chunk1 = new Uint8Array(MAX_MARKETPLACE_INDEX_BYTES).fill(120)
    const chunk2 = new Uint8Array(1024).fill(120) // total > MAX_MARKETPLACE_INDEX_BYTES
    let canceled = false

    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        controller.enqueue(chunk1)
        controller.enqueue(chunk2)
      },
      cancel() {
        canceled = true
      },
    })

    const response = new Response(stream, { status: 200 })
    await assert.rejects(
      fetchMarketplaceIndex(
        'https://example.com/index.json',
        async () => response
      ),
      (err: unknown) => {
        assert.ok(err instanceof MarketplaceIndexFetchError)
        assert.equal(err.reason, 'too_large')
        return true
      }
    )
    assert.equal(canceled, true)
  })

  test('formatMarketplaceError produces localized error messages for all failure reasons', () => {
    const fakeT = (key: string, opts?: Record<string, unknown>) => {
      if (opts?.status) return `HTTP_${opts.status}`
      return `TRANSLATED_${key}`
    }

    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('unreachable'),
        fakeT
      ),
      'TRANSLATED_Network error or host unreachable'
    )
    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('not_found', 404),
        fakeT
      ),
      'HTTP_404'
    )
    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('too_large'),
        fakeT
      ),
      'TRANSLATED_Index payload exceeds size limit (2 MiB)'
    )
    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('invalid_json'),
        fakeT
      ),
      'TRANSLATED_Invalid JSON in index response'
    )
    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('invalid_structure'),
        fakeT
      ),
      'TRANSLATED_Index payload structure is invalid'
    )
    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('unsupported_version'),
        fakeT
      ),
      'TRANSLATED_Unsupported index version'
    )
    assert.equal(
      formatMarketplaceError(
        new MarketplaceIndexFetchError('oversized_catalog'),
        fakeT
      ),
      'TRANSLATED_Index contains too many plugins (exceeds 500)'
    )
  })
})
