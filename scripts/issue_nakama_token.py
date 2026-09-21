#!/usr/bin/env python3
"""Mint a one-hour Nakama ServiceAccount credential into a private local bundle."""
import argparse
import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

from k3s_node import SetupError, absolute_path, atomic_write, encode, label, secure_read, server_url


def validate_response(response, namespace, service_account, now):
    status=response.get('status',{})
    token=status.get('token','')
    if not isinstance(token,str) or len(token)>32768 or not re.fullmatch(r'[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+',token):
        raise SetupError('API did not return a bounded ServiceAccount JWT.')
    expires=datetime.fromisoformat(status.get('expirationTimestamp','').replace('Z','+00:00'))
    if expires.tzinfo is None or not 1200 <= (expires-now).total_seconds() <= 7200:
        raise SetupError('Issued token lifetime is outside the expected 20-minute to 2-hour window.')
    payload=token.split('.')[1]
    claims=json.loads(base64.urlsafe_b64decode(payload+'='*((-len(payload))%4)))
    if claims.get('sub')!='system:serviceaccount:'+namespace+':'+service_account:
        raise SetupError('Issued token identifies an unexpected ServiceAccount.')
    if abs(float(claims.get('exp',0))-expires.timestamp())>1:
        raise SetupError('Issued token expiration metadata is inconsistent.')
    # This is a structural check of a TLS-authenticated API response, not local
    # JWT signature verification. The Kubernetes API verifies the token on use.
    return token,expires.isoformat().replace('+00:00','Z')


def issue(args):
    if os.geteuid()!=0:raise SetupError('Run the issuer as root on the control node.')
    secure_read(args.kubeconfig)
    if not any(args.output.is_relative_to(root) for root in (Path('/var/lib/nakama-agones'),Path('/run/nakama-agones'),Path('/root'))):
        raise SetupError('Credential output must stay under /var/lib/nakama-agones, /run/nakama-agones or /root, outside a source checkout.')
    # A fixed private export directory can be served by a restricted SSH forced
    # command. Nothing is sent to a remote machine by this script.
    args.output.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    parent=args.output.parent.lstat()
    if not stat.S_ISDIR(parent.st_mode) or parent.st_uid!=0 or stat.S_IMODE(parent.st_mode)!=0o700:
        raise SetupError('Token bundle directory must be a root-owned, non-symlink 0700 directory.')
    if args.output.exists():secure_read(args.output)
    endpoint='/api/v1/namespaces/'+args.namespace+'/serviceaccounts/'+args.service_account+'/token'
    request={'apiVersion':'authentication.k8s.io/v1','kind':'TokenRequest','spec':{'audiences':[],'expirationSeconds':3600}}
    command=[str(args.k3s),'kubectl','--kubeconfig',str(args.kubeconfig),'--server',args.api_url,
             '--request-timeout=15s','create','--raw',endpoint,'-f','-']
    child=subprocess.run(command,input=encode(request),stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=25,
                         env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'})
    if child.returncode:raise SetupError('TokenRequest failed. Previous export is retained; check API reachability and issuer permissions.')
    if len(child.stdout)>65536:raise SetupError('TokenRequest response exceeded its size limit.')
    token,expires=validate_response(json.loads(child.stdout),args.namespace,args.service_account,datetime.now(timezone.utc))
    bundle={'schema':1,'api_url':args.api_url,'namespace':args.namespace,'service_account':args.service_account,
            'token':token,'expires_at':expires}
    atomic_write(args.output,encode(bundle))
    print('Nakama ServiceAccount credential refreshed in the private 0600 bundle; expires '+expires+'.')


def main():
    os.umask(0o077)
    cli=argparse.ArgumentParser(description=__doc__)
    cli.add_argument('--k3s',type=absolute_path,default=Path('/usr/local/bin/k3s'))
    cli.add_argument('--kubeconfig',type=absolute_path,required=True)
    cli.add_argument('--api-url',type=server_url,required=True)
    cli.add_argument('--namespace',type=label,default='agones-control')
    cli.add_argument('--service-account',type=label,default='nakama-agones')
    cli.add_argument('--output',type=absolute_path,required=True)
    try:issue(cli.parse_args())
    except (SetupError,OSError,ValueError,TypeError,KeyError,subprocess.TimeoutExpired) as error:
        print(str(error) if isinstance(error,SetupError) else 'Token issuance failed; previous export retained. No credentials are printed.',file=sys.stderr)
        return 1
    return 0


if __name__=='__main__':sys.exit(main())
