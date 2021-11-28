# Pixiu autoscaler

## Installing
`pixiu-autoscaler` 控制器的安装非常简单，通过 `kubectl` 执行 `apply` 如下文件即可完成安装，真正做到猩猩都能使用.

``` bash
kubectl apply -f pixiu-autoscaler-controller.yaml
```

然后通过 `kubectl get pod -l pixiu.hpa.controller=pixiu-autoscaler-controller -n kube-system` 能看到 `pixiu-autoscaler` 已经启动成功.

``` bash
NAME                                          READY   STATUS    RESTARTS   AGE
pixiu-autoscaler-controller-dbc4bc4d8-hwpqz   1/1     Running   0          20s
pixiu-autoscaler-controller-dbc4bc4d8-tqxrl   1/1     Running   0          20s
```

## Tests

通过 `example` 安装测试 `workload`

``` bash
kubectl apply -f deployment-example.yaml
```

通过 `kubectl get hpa test1` 命令，可以看到 `deployment` / `statefulset` 关联的 `HPA` 被自动创建

``` bash
NAME    REFERENCE          TARGETS            MINPODS   MAXPODS   REPLICAS   AGE
test1   Deployment/test1   6% / 60%           1         2         1          5h29m
test2   Deployment/test2   152%/50%, 0%/60%   2         3         3          34m
```