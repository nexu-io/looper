#!/usr/bin/env python3
"""Task-scoped Looper tools: no Plane/Feishu credentials are exposed to the Agent."""
import argparse
import http.client
import json
import socket
import sys


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=90)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def call_host(socket_path, task, payload):
    if not isinstance(task, str) or not task or '/' in task or len(task) > 180:
        raise ValueError('valid task ID required')
    conn = UnixHTTP(socket_path)
    conn.request('POST', '/memory/' + task, json.dumps(payload), {'Content-Type':'application/json'})
    response = conn.getresponse()
    body = response.read(4 * 1024 * 1024).decode('utf-8')
    conn.close()
    return response.status, body


def mcp(socket_path, bound_task):
    schemas = {
        'list_for_item': ({'cursor': {'type':'integer','minimum':0}, 'include_project_modules':{'type':'boolean'}}, []),
        'get': ({'memory_id':{'type':'string'}, 'revision':{'type':'integer','minimum':1}, 'cursor':{'type':'integer','minimum':0}, 'logical_read_id':{'type':'string'}}, ['memory_id','revision']),
        'feedback': ({'memory_id':{'type':'string'}, 'revision':{'type':'integer','minimum':1}, 'result':{'type':'string','enum':['adopted','helpful','incorrect','inapplicable']}, 'purpose':{'type':'string'}, 'basis':{'type':'string','enum':['index','body']}}, ['memory_id','revision','result','purpose']),
        'propose': ({'proposal':{'type':'object'}}, ['proposal']),
    }
    descriptions = {
        'list_for_item': '查看当前 Plane item 的项目和 module 记忆索引，支持明确分页及展开同项目其他 module。',
        'get': '读取指定发布修订的记忆原文，保留出处、内容 hash 和实际返回范围。',
        'feedback': '声明采用用途或反馈错误、不适用、有帮助；声明不代表独立验证。',
        'propose': '需配套 Plane 个人记忆接口：提交有来源的个人经验，完整内容可供本人后续任务参考；不能共享或覆盖项目知识。',
    }
    tools = []
    for name, (properties, required) in schemas.items():
        properties = dict(properties)
        if not bound_task:
            properties['task'] = {'type':'string','description':'当前 Looper 任务 ID，从任务上下文取得'}
            required = ['task'] + required
        tools.append({'name':name,'description':descriptions[name], 'inputSchema':{
            'type':'object','properties':properties,'required':required,'additionalProperties':False}})
    for line in sys.stdin:
        message = {}
        try:
            message = json.loads(line)
            if not isinstance(message, dict):
                raise ValueError('JSON-RPC object required')
            method = message.get('method')
            if 'id' not in message:
                continue
            if method == 'initialize':
                result = {'protocolVersion':message.get('params',{}).get('protocolVersion','2024-11-05'),
                          'capabilities':{'tools':{}},'serverInfo':{'name':'looper-memory','version':'1.0.0'}}
            elif method == 'ping':
                result = {}
            elif method == 'tools/list':
                result = {'tools':tools}
            elif method == 'tools/call':
                params = message.get('params',{})
                if not isinstance(params,dict) or not isinstance(params.get('arguments',{}),dict):
                    raise ValueError('tool arguments must be an object')
                name = params.get('name')
                if not isinstance(name,str) or name not in schemas:
                    raise ValueError('unknown memory tool')
                payload = dict(params.get('arguments',{}))
                task = bound_task or payload.pop('task',None)
                payload['action'] = name
                status, text = call_host(socket_path,task,payload)
                result = {'content':[{'type':'text','text':text}], 'isError':status != 200}
            else:
                raise ValueError('unsupported method')
            response = {'jsonrpc':'2.0','id':message['id'],'result':result}
        except (ValueError,OSError,http.client.HTTPException) as error:
            if not isinstance(message,dict) or 'id' not in message:
                continue
            response = {'jsonrpc':'2.0','id':message['id'],'error':{'code':-32602,'message':str(error)}}
        print(json.dumps(response,ensure_ascii=False),flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--socket', required=True)
    parser.add_argument('--task')
    parser.add_argument('--mcp',action='store_true',help='Expose task-scoped memory tools via MCP stdio')
    parser.add_argument('action', nargs='?', choices=['list_for_item','get','feedback','propose'])
    parser.add_argument('--input',help='JSON object file, or - for stdin',default='-')
    args=parser.parse_args()
    if args.mcp:
        mcp(args.socket,args.task)
        return 0
    if not args.task or not args.action:
        parser.error('--task and action are required outside MCP mode')
    payload = json.load(sys.stdin if args.input == '-' else open(args.input,encoding='utf-8')) if (args.input!='-' or not sys.stdin.isatty()) else {}
    if not isinstance(payload,dict):
        parser.error('input must be a JSON object')
    payload['action']=args.action
    status,body=call_host(args.socket,args.task,payload)
    print(body)
    return 0 if status==200 else 1


if __name__=='__main__':
    try:
        sys.exit(main())
    except (OSError,ValueError,http.client.HTTPException) as error:
        print(json.dumps({'error':str(error)},ensure_ascii=False))
        sys.exit(1)
