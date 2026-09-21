#!/usr/bin/env python3
"""Read private cutover snapshots; print only counts and exact-equality results."""
import argparse
from collections import Counter
import json
import os
from pathlib import Path
import stat
import sys
import time

TERMINAL={'completed','cancelled','expired','failed'}
WORKER_STATES={'requested','starting','unknown','launching','bootstrapping','ready','suspect','draining','stopping','stopped','failed','lost'}
ALLOCATION_STATES=TERMINAL|{'waiting_capacity','preparing','assigned','active','closing'}
CONFIG_FIELDS={'nakama_database_addresses','fleet_database_url','control_url','nakama_server_key','session_encryption_key','refresh_encryption_key'}


class AuditError(Exception):pass


def unique(pairs):
    result={}
    for key,value in pairs:
        if key in result:raise AuditError('Snapshot contains duplicate fields.')
        result[key]=value
    return result


def read_private(path):
    with os.fdopen(os.open(path,os.O_RDONLY|os.O_NOFOLLOW),'rb') as stream:
        info=os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or stat.S_IMODE(info.st_mode)!=0o600:
            raise AuditError('Snapshots must be owner-only regular 0600 files.')
        if not 0<info.st_size<=32<<20:raise AuditError('Snapshot size is invalid.')
        return json.loads(stream.read((32<<20)+1),object_pairs_hook=unique)


def number(value):
    if type(value)!=int or value<0:raise AuditError('Snapshot contains an invalid counter.')
    return value


def inspect_state(snapshot,now):
    if not isinstance(snapshot,dict) or not all(isinstance(snapshot.get(name),dict) for name in ('workers','allocations','commands')):
        raise AuditError('Expected one complete Fleet state snapshot containing workers, allocations and commands.')
    workers=Counter();allocations=Counter();unretired=0;nonterminal=0;pending=0;players=0;work=0
    for worker in snapshot['workers'].values():
        if not isinstance(worker,dict) or not isinstance(worker.get('state'),str) or not isinstance(worker.get('provider_id',''),str):
            raise AuditError('Snapshot contains an invalid worker.')
        phase=worker['state'];workers[phase if phase in WORKER_STATES else 'unrecognized']+=1
        if phase!='stopped' and not (phase=='failed' and not worker.get('provider_id')):unretired+=1
        players+=number(worker.get('player_count',0))
        metrics=worker.get('metrics',{})
        if not isinstance(metrics,dict):raise AuditError('Snapshot contains invalid worker metrics.')
        work+=sum(number(metrics.get(key,0)) for key in ('simulation_active','simulation_pending','audit_active','audit_pending','pending_results'))
    for allocation in snapshot['allocations'].values():
        if not isinstance(allocation,dict) or not isinstance(allocation.get('state'),str):raise AuditError('Snapshot contains an invalid allocation.')
        phase=allocation['state'];allocations[phase if phase in ALLOCATION_STATES else 'unrecognized']+=1
        if phase not in TERMINAL:nonterminal+=1
    for command in snapshot['commands'].values():
        if not isinstance(command,dict) or type(command.get('done',False))!=bool:raise AuditError('Snapshot contains an invalid command.')
        if not command.get('done',False) and number(command.get('expires_at',0))>now:pending+=1
    return {'workers_by_state':dict(workers),'allocations_by_state':dict(allocations),
        'unretired_workers':unretired,'nonterminal_allocations':nonterminal,'unexpired_pending_commands':pending,
        'last_reported_players':players,'last_reported_pending_work':work,
        'retirement_state_complete':unretired==0 and nonterminal==0 and pending==0,
        'provider_inventory_verified':False,'old_controller_shutdown_verified':False}


def compare_config(before,after):
    for value in (before,after):
        if not isinstance(value,dict) or set(value)!=CONFIG_FIELDS:raise AuditError('Configuration snapshot does not match the documented schema.')
        addresses=value['nakama_database_addresses']
        if not isinstance(addresses,list) or not addresses or not all(isinstance(item,str) and item for item in addresses):
            raise AuditError('Configuration snapshot has invalid database addresses.')
        if not all(isinstance(value[name],str) and value[name] for name in CONFIG_FIELDS-{'nakama_database_addresses'}):
            raise AuditError('Configuration snapshot has missing identity settings.')
    # Compare the original strings exactly, without parsing, reordering or
    # normalizing DSNs; never emit their contents or their hashes.
    return {name:before[name]==after[name] for name in sorted(CONFIG_FIELDS)}


def main():
    cli=argparse.ArgumentParser(description=__doc__)
    cli.add_argument('--state-file',type=Path,required=True)
    cli.add_argument('--before-config',type=Path)
    cli.add_argument('--after-config',type=Path)
    try:
        args=cli.parse_args()
        if bool(args.before_config)!=bool(args.after_config):raise AuditError('Supply both before-config and after-config for exact preservation checks.')
        result=inspect_state(read_private(args.state_file),int(time.time()))
        result['configuration_check_supplied']=bool(args.before_config)
        same=True
        if args.before_config:
            comparisons=compare_config(read_private(args.before_config),read_private(args.after_config))
            result['configuration_unchanged']=comparisons;same=all(comparisons.values())
        print(json.dumps(result,sort_keys=True,indent=2))
        return 0 if result['retirement_state_complete'] and same else 2
    except (AuditError,OSError,ValueError,TypeError):
        error=sys.exc_info()[1]
        print(str(error) if isinstance(error,AuditError) else 'Cutover audit failed; no snapshot contents are printed.',file=sys.stderr)
        return 1


if __name__=='__main__':sys.exit(main())
