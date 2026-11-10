# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Consolidate completed offline analyses with paired run-level uncertainty."""

import csv
import hashlib
import json
import math
from pathlib import Path
import statistics

OUT = Path(__file__).resolve().parent
REPO = OUT.parent
SOURCES = {
    '0.6B': OUT / '0.6b/metrics-comparison.json',
    '8B': OUT / '8b/metrics-comparison.json',
}


def estimate(ratios):
    """Return a geometric paired effect and 95% t interval for five ratios."""
    if len(ratios) != 5 or any(not math.isfinite(x) or x <= 0 for x in ratios):
        raise ValueError('Expected five finite positive paired ratios')
    logs = [math.log(x) for x in ratios]
    mean = statistics.mean(logs)
    half = 2.7764451051977987 * statistics.stdev(logs) / math.sqrt(5)
    return {'change_pct': 100 * math.expm1(mean),
            'ci95_pct': [100 * math.expm1(mean - half), 100 * math.expm1(mean + half)]}


def main():
    """Validate paired source estimates and write consolidated JSON and CSV."""
    rows = []
    filename = OUT / 'metrics-comparison.json'
    data = json.loads(filename.read_text(encoding='utf-8'))
    if data['accepted_cells'] != 540:
        raise ValueError('Incomplete 27B source')
    for model, source in {**SOURCES, '27B FP8': filename}.items():
        data = json.loads(source.read_text(encoding='utf-8'))
        if data['accepted_cells'] != 540:
            raise ValueError('Incomplete source: ' + model)
        runs = {r['cell']: r for r in data['runs']}
        for comparison in data['comparisons']:
            for metric, value in comparison['metrics'].items():
                ratios = []
                for pair in comparison['provenance']:
                    a = runs[pair['numerator_cell']]['metrics'].get(metric)
                    b = runs[pair['denominator_cell']]['metrics'].get(metric)
                    if isinstance(a, (int, float)) and isinstance(b, (int, float)) and a > 0 and b > 0:
                        ratios.append(a / b)
                if len(ratios) != 5:
                    continue
                interval = estimate(ratios)
                if abs(interval['change_pct'] - value['change_pct']) > 1e-8:
                    raise ValueError('Point estimate differs from raw analysis')
                rows.append({'model': model, **{k: comparison[k] for k in
                    ('path', 'isl', 'osl', 'concurrency', 'comparison', 'pairs', 'same_gpu_pairs', 'sparse_p99_pairs')},
                    'metric': metric, 'change_pct': interval['change_pct'],
                    'ci95_low_pct': interval['ci95_pct'][0], 'ci95_high_pct': interval['ci95_pct'][1]})
    for row in rows:
        row['primary_workload'] = (row['isl'], row['osl'], row['concurrency']) == (128, 16, 128)
        row['interval_excludes_zero'] = row['ci95_low_pct'] > 0 or row['ci95_high_pct'] < 0
    with (OUT / 'all-model-paired-metrics.csv').open('w', newline='', encoding='utf-8') as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    sources = [*SOURCES.values(), filename]
    result = {'accepted_main_sweep_cells': 1620,
              'missing_metric_policy': 'Comparisons without five positive finite metric pairs omitted; missing token-spacing observations are not zero',
              'method': 'Five paired log run ratios; individual 95% t intervals; secondary metrics exploratory and unadjusted',
              'cpu_method': data['cpu_method'],
              'source_sha256': {str(p.relative_to(REPO)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources},
              'comparisons': rows}
    (OUT / 'all-model-paired-metrics.json').write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8', newline='\n')
    print(json.dumps({'comparison_metric_rows': len(rows), 'primary_rows': sum(r['primary_workload'] for r in rows)}))
    metrics = ['latency_avg_ms', 'latency_p99_ms', 'ttft_avg_ms', 'itl_avg_ms', 'serving_cpu_ms_per_request']
    for model in [*SOURCES, '27B FP8']:
        for path in ['vllm', 'sglang', 'dynamo-vllm']:
            for contrast in ['blocked / allowed', 'restored-blocked / blocked']:
                selected = [r for r in rows if r['primary_workload'] and r['model'] == model
                            and r['path'] == path and r['comparison'] == contrast and r['metric'] in metrics]
                print(model, path, contrast, json.dumps({r['metric']: [round(r[k], 3) for k in
                      ('change_pct', 'ci95_low_pct', 'ci95_high_pct')] for r in selected}))


if __name__ == '__main__':
    main()
