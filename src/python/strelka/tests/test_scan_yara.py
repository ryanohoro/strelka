from pathlib import Path
from unittest import TestCase, mock

from strelka.scanners.scan_yara import ScanYara as ScanUnderTest
from strelka.tests import run_test_scan


def test_scan_yara(mocker):
    """
    Pass: Sample event matches output of scanner.
    Failure: Unable to load file or sample event fails to match.
    """

    test_scan_event = {
        "elapsed": mock.ANY,
        "flags": [],
        "matches": ["test"],
        "meta": [],
        "tags": [],
    }

    scanner_event = run_test_scan(
        mocker=mocker,
        scan_class=ScanUnderTest,
        fixture_path=Path(__file__).parent / "fixtures/test.txt",
        options={"location": str(Path(Path(__file__).parent / "fixtures/test.yara"))},
    )

    TestCase.maxDiff = None
    TestCase().assertDictEqual(test_scan_event, scanner_event)
