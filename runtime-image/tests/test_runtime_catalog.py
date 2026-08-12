import copy
import hashlib
import json
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))

from runtime_catalog import BASELINE_REQUIREMENTS, CatalogError, build_catalog, canonical_json
from validate_runtime_catalog import validate

FIXTURES = Path(__file__).parent / "fixtures"


def fixture(name):
    return json.loads((FIXTURES / name).read_text(encoding="utf-8"))


def runtime_manifest():
    return {
        "files": {
            "bin": {"type": "directory"},
            "bin/java": {
                "downloads": {
                    "raw": {
                        "sha1": "a" * 40,
                        "sha256": "b" * 64,
                        "size": 1,
                        "url": "https://piston-data.mojang.com/v1/objects/a/java",
                    },
                },
                "executable": True,
                "type": "file",
            },
        },
    }


def fixture_fetch(version_manifest, all_runtimes):
    documents = {}
    for version in version_manifest["versions"]:
        component = version["id"]
        document = {"javaVersion": {"component": "jre-legacy" if component == "legacy" else f"java-runtime-{component}", "majorVersion": {"legacy": 8, "alpha": 16, "gamma": 17, "delta": 21, "epsilon": 25}[component]}}
        data = canonical_json(document)
        version["sha1"] = hashlib.sha1(data).hexdigest()
        documents[version["url"]] = data
    for component, records in all_runtimes["linux"].items():
        for record in records:
            data = canonical_json(runtime_manifest())
            record["manifest"]["sha1"] = hashlib.sha1(data).hexdigest()
            documents[record["manifest"]["url"]] = data
    return lambda url: documents[url]


class RuntimeCatalogResolutionTests(unittest.TestCase):
    def test_prefers_usable_official_runtime_over_zulu(self):
        versions = fixture("minecraft-version-manifest.json")
        runtimes = fixture("java-runtime-all.json")
        catalog = build_catalog(versions, runtimes, fixture("zulu-fallback.json"), fixture_fetch(versions, runtimes))

        self.assertEqual({item["major"] for item in catalog["runtimes"]}, {8, 16, 17, 21, 25})
        gamma = next(item for item in catalog["resolutions"] if item["component"] == "java-runtime-gamma")
        self.assertEqual(gamma["source"], "mojang")
        self.assertIn({"component": "java-runtime-epsilon", "major": 25}, catalog["requirements"])
        self.assertNotIn(("java-runtime-epsilon", 25), BASELINE_REQUIREMENTS)
        validate(catalog)

    def test_uses_zulu_only_when_official_component_is_not_usable(self):
        versions = fixture("minecraft-version-manifest.json")
        runtimes = fixture("java-runtime-all.json")
        runtimes["linux"]["java-runtime-gamma"][0]["availability"]["progress"] = 20
        catalog = build_catalog(versions, runtimes, fixture("zulu-fallback.json"), fixture_fetch(versions, runtimes))

        gamma = next(item for item in catalog["resolutions"] if item["component"] == "java-runtime-gamma")
        self.assertEqual(gamma["source"], "zulu")
        validate(catalog)

    def test_rejects_unusable_official_metadata_without_downgrading(self):
        versions = fixture("minecraft-version-manifest.json")
        runtimes = fixture("java-runtime-all.json")
        broken = runtime_manifest()
        del broken["files"]["bin/java"]
        fetch = fixture_fetch(versions, runtimes)
        gamma_url = runtimes["linux"]["java-runtime-gamma"][0]["manifest"]["url"]
        documents = {url: fetch(url) for url in [version["url"] for version in versions["versions"]]}
        broken_data = canonical_json(broken)
        runtimes["linux"]["java-runtime-gamma"][0]["manifest"]["sha1"] = hashlib.sha1(broken_data).hexdigest()
        documents[gamma_url] = broken_data

        with self.assertRaises(CatalogError):
            build_catalog(versions, runtimes, fixture("zulu-fallback.json"), lambda url: documents[url] if url in documents else fetch(url))

    def test_output_is_deterministic(self):
        versions = fixture("minecraft-version-manifest.json")
        runtimes = fixture("java-runtime-all.json")
        fetch = fixture_fetch(versions, runtimes)
        first = build_catalog(copy.deepcopy(versions), copy.deepcopy(runtimes), fixture("zulu-fallback.json"), fetch)
        second = build_catalog(copy.deepcopy(versions), copy.deepcopy(runtimes), fixture("zulu-fallback.json"), fetch)
        self.assertEqual(canonical_json(first), canonical_json(second))


if __name__ == "__main__":
    unittest.main()
