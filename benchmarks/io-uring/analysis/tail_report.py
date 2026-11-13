# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Recompute paired follow-up estimates from packaged per-run window metrics."""

import hashlib
import json
import math
from pathlib import Path
import statistics

ROOT = Path(__file__).resolve().parent
T95 = {1: 12.706204736, 2: 4.302652730, 3: 3.182446305, 4: 2.776445105,
       5: 2.570581836, 6: 2.446911851, 7: 2.364624252, 8: 2.306004135, 9: 2.262157163}
WINDOWS = {'early': (0, 90), 'steady': (120, 300), 'full': (0, 300)}
PLANNED_PAIRS = 10
# The original segment's execution_segments events record prolonged idle before
# conditioning for repeats 2 and 5. Both conditions in each pair share a repeat.
RECOVERY_IDLE_REPEATS = (2, 5)


def percentile(values, fraction):
    """Linearly interpolate a sample percentile; reject an empty sample."""
    values = sorted(values)
    if not values:
        raise ValueError('Empty percentile sample')
    position = (len(values) - 1) * fraction
    lower, upper = math.floor(position), math.ceil(position)
    return values[lower] + (values[upper] - values[lower]) * (position - lower)


def interval(ratios, *, confirmatory=False):
    """Estimate paired log-ratio uncertainty; confirmation requires ten pairs."""
    if not ratios or any(not math.isfinite(v) or v <= 0 for v in ratios):
        raise ValueError('Expected positive finite paired ratios')
    logs = [math.log(v) for v in ratios]
    average = statistics.mean(logs)
    result = {'pairs': len(logs), 'ratio': math.exp(average), 'individual_ratios': ratios,
              'ci95': None, 'interpretation': 'incomplete; no confirmatory conclusion'}
    if len(logs) >= 2 and (not confirmatory or len(logs) == PLANNED_PAIRS):
        half = T95[len(logs) - 1] * statistics.stdev(logs) / math.sqrt(len(logs))
        low, high = math.exp(average - half), math.exp(average + half)
        result['ci95'] = [low, high]
        result['interpretation'] = ('interval indicates more than 3% higher latency' if low > 1.03 else
                                    'interval excludes more than 3% higher latency' if high < 1.03 else
                                    'inconclusive for the 3% higher-latency margin')
    return result


def metric(record, name):
    """Read a request metric, rejecting latency units other than milliseconds."""
    value = record['metrics'][name]
    if value['unit'] != 'ms':
        raise ValueError('Unexpected latency units: ' + name)
    return value['value']


def window_metrics(records, low, high):
    """Summarize request-relative windows and gaps only where observed.

    The first profiling request approximates phase start. The packaged inputs
    lack the actual phase-start timestamp, so the offset and its effect on
    boundary membership cannot be determined from this package.
    """
    origin = min(r['metadata']['request_start_ns'] for r in records)
    selected = [r for r in records
                if low <= (r['metadata']['request_start_ns'] - origin) / 1e9 < high]
    if not selected:
        raise ValueError('No requests in prespecified window')
    result = {'requests': len(selected), 'request_start_window_s': [low, high],
              'sparse_p99': len(selected) < 1000,
              'request_start_rate_per_second': len(selected) / (high - low)}
    for name in ('request_latency', 'time_to_first_token', 'decode_duration'):
        values = [metric(r, name) for r in selected]
        result[name + '_p99_ms'] = percentile(values, .99)
        if name == 'request_latency':
            result[name + '_p95_ms'] = percentile(values, .95)
    gaps = [max(metric(r, 'inter_chunk_latency')) for r in selected
            if r['metrics'].get('inter_chunk_latency', {}).get('value')]
    result['largest_chunk_gap_p99_ms'] = percentile(gaps, .99) if gaps else None
    result['requests_with_observed_chunk_gaps'] = len(gaps)
    result['requests_without_observed_chunk_gaps'] = len(selected) - len(gaps)
    return result


def generate():
    """Verify both packaged segment inputs and recompute paired estimates.

    ROOT/tail/continuation-source.json lists input files relative to its own
    directory, with a SHA-256 for each. Those files contain saved per-run window
    metrics, not raw requests; this verifies the packaged analysis inputs and
    recomputes run-level comparisons, not request-level windows or percentiles.
    """
    source_file = ROOT / 'tail/continuation-source.json'
    try:
        source = json.loads(source_file.read_text(encoding='utf-8'))
    except FileNotFoundError as error:
        raise FileNotFoundError(
            f'Missing packaged input index: {source_file}. Keep tail_report.py '
            'alongside tail/ and its indexed per-run metric files.'
        ) from error
    segments = []
    for entry in source['segments']:
        filename = source_file.parent / entry['file']
        raw = filename.read_bytes()
        if hashlib.sha256(raw).hexdigest() != entry['sha256']:
            raise RuntimeError(f'Packaged analysis input changed: {filename}')
        segments.append(json.loads(raw))
    total_cells = sum(segment['accepted_cells'] for segment in segments)
    if total_cells != source['protocol_total_cells']:
        raise ValueError('Segment cell counts do not match the protocol total')
    rows = [run for segment in segments for run in segment['runs']]
    pairs = []
    identities = [(row['repeat'], row['condition']) for row in rows]
    if len(identities) != len(set(identities)):
        raise RuntimeError('Duplicate repeat/condition across benchmark segments')
    for repeat in range(1, PLANNED_PAIRS + 1):
        conditions = {r['condition']: r for r in rows if r['repeat'] == repeat}
        if set(conditions) == {'blocked', 'restored-blocked'}:
            pairs.append(conditions)
    comparisons = []
    if pairs:
        metrics = ('request_latency_p99_ms', 'request_latency_p95_ms', 'time_to_first_token_p99_ms',
                   'decode_duration_p99_ms', 'largest_chunk_gap_p99_ms')
        for window in WINDOWS:
            for name in metrics:
                primary = window == 'steady' and name == 'request_latency_p99_ms'
                usable = [p for p in pairs if p['restored-blocked']['windows'][window][name] is not None
                          and p['blocked']['windows'][window][name] is not None]
                ratios = [p['restored-blocked']['windows'][window][name] /
                          p['blocked']['windows'][window][name] for p in usable]
                estimate = interval(ratios, confirmatory=primary) if ratios else {'pairs': 0, 'ratio': None, 'ci95': None}
                comparisons.append({'window': window, 'metric': name, 'primary': primary, **estimate})
        same = [p for p in pairs if p['blocked']['gpu_uuid'] == p['restored-blocked']['gpu_uuid']]
        sensitivity = interval([p['restored-blocked']['windows']['steady']['request_latency_p99_ms'] /
                                p['blocked']['windows']['steady']['request_latency_p99_ms'] for p in same]) if same else None
        if sensitivity is not None:
            sensitivity.pop('interpretation')  # Exploratory subsets do not give a confirmatory verdict.
        throughput = interval([p['restored-blocked']['requests_per_second'] /
                               p['blocked']['requests_per_second'] for p in pairs])
        throughput.pop('interpretation')  # Latency and throughput margins have opposite directions.
    else:
        same, sensitivity, throughput = [], None, None
    result = {'state': segments[-1]['state'], 'accepted_cells': total_cells, 'planned_cells': 80,
              'accepted_primary_profiles': len(rows), 'planned_primary_profiles': 20,
              'complete_pairs': len(pairs), 'comparisons': comparisons, 'runs': rows,
              'same_gpu_pairs': len(same), 'same_gpu_steady_p99_sensitivity': sensitivity,
              'full_profile_throughput_secondary': throughput,
              'execution_segments': [segment['execution_segment'] for segment in segments],
              'method': 'Paired log run-level ratios; 95% Student-t intervals; primary conclusion requires all 10 pairs',
              'timing': 'Windows use request start relative to first profiling request after 30s warmup; '
                        'approximation of profiling-phase start, whose timestamp is unavailable in packaged inputs; '
                        'boundary membership may differ; not time since restore',
              'caveats': ['Secondary comparisons exploratory and unadjusted', 'Same-GPU sensitivity may have too few pairs',
                          'Fixed C1→8→32→128 conditioning; cannot infer all prior workload histories',
                          'Chunk-gap secondary metric is conditional on observed inter-chunk gaps; absence is not zero',
                          'Repeats 2 and 5 have recorded recovery idle intervals; inspect sensitivity excluding those pairs']}
    unaffected = [p for p in pairs if p['blocked']['repeat'] not in RECOVERY_IDLE_REPEATS]
    result['excluding_recovery_pairs_steady_p99_exploratory'] = (
        interval([p['restored-blocked']['windows']['steady']['request_latency_p99_ms'] /
                  p['blocked']['windows']['steady']['request_latency_p99_ms'] for p in unaffected])
        if unaffected else None)
    if result['excluding_recovery_pairs_steady_p99_exploratory'] is not None:
        result['excluding_recovery_pairs_steady_p99_exploratory'].pop('interpretation')
    temporary = ROOT / 'tail-results.json.tmp'
    temporary.write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8', newline='\n')
    temporary.replace(ROOT / 'tail-results.json')
    return result


if __name__ == '__main__':
    report = generate()
    print(json.dumps({key: report[key] for key in ('state', 'accepted_cells', 'complete_pairs')}))
