package k8s

import (
	"encoding/json"
	"io"

	"github.com/luizalabs/teresa-api/pkg/server/app"
	st "github.com/luizalabs/teresa-api/pkg/server/storage"
	"github.com/pkg/errors"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/resource"
	k8sv1 "k8s.io/client-go/pkg/api/v1"
	asv1 "k8s.io/client-go/pkg/apis/autoscaling/v1"
	restclient "k8s.io/client-go/rest"
)

type k8sClient struct {
	kc *kubernetes.Clientset
}

func (k *k8sClient) Create(app *app.App, st st.Storage) error {
	panic("not implemented")
}

func (k *k8sClient) getNamespace(namespace string) (*k8sv1.Namespace, error) {
	ns, err := k.kc.CoreV1().Namespaces().Get(namespace)
	if err != nil {
		return nil, err
	}
	return ns, nil
}

func (k *k8sClient) NamespaceAnnotation(namespace, annotation string) (string, error) {
	ns, err := k.getNamespace(namespace)
	if err != nil {
		return "", errors.Wrap(err, "get annotation failed")
	}

	return ns.Annotations[annotation], nil
}

func (k k8sClient) NamespaceLabel(namespace, label string) (string, error) {
	ns, err := k.getNamespace(namespace)
	if err != nil {
		return "", errors.Wrap(err, "get label failed")
	}

	return ns.Labels[label], nil
}

func (k *k8sClient) PodList(namespace string) ([]*app.Pod, error) {
	podList, err := k.kc.CoreV1().Pods(namespace).List(k8sv1.ListOptions{})
	if err != nil {
		return nil, err
	}

	pods := make([]*app.Pod, 0)
	for _, pod := range podList.Items {
		p := &app.Pod{Name: pod.Name}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Waiting != nil {
				p.State = status.State.Waiting.Reason
			} else if status.State.Terminated != nil {
				p.State = status.State.Terminated.Reason
			} else if status.State.Running != nil {
				p.State = string(api.PodRunning)
			}
			if p.State != "" {
				break
			}
		}
		pods = append(pods, p)
	}
	return pods, nil
}

func (k *k8sClient) PodLogs(namespace string, podName string, lines int64, follow bool) (io.ReadCloser, error) {
	req := k.kc.CoreV1().Pods(namespace).GetLogs(
		podName,
		&k8sv1.PodLogOptions{
			Follow:    follow,
			TailLines: &lines,
		},
	)

	return req.Stream()
}

func newNs(a *app.App, user string) *k8sv1.Namespace {
	return &k8sv1.Namespace{
		ObjectMeta: k8sv1.ObjectMeta{
			Name: a.Name,
			Labels: map[string]string{
				"teresa.io/team": a.Team,
			},
			Annotations: map[string]string{
				"teresa.io/last-user": user,
			},
		},
	}
}

func addAppToNs(a *app.App, ns *k8sv1.Namespace) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}

	ns.Annotations["teresa.io/app"] = string(b)
	return nil
}

func addLimitRangeQuantityToResourceList(r *k8sv1.ResourceList, lrQuantity []*app.LimitRangeQuantity) error {
	if lrQuantity == nil {
		return nil
	}

	rl := k8sv1.ResourceList{}
	for _, item := range lrQuantity {
		name := k8sv1.ResourceName(item.Resource)
		q, err := resource.ParseQuantity(item.Quantity)
		if err != nil {
			return err
		}
		rl[name] = q
	}
	*r = rl
	return nil
}

func parseLimitRangeParams(lrItem *k8sv1.LimitRangeItem, lim *app.Limits) error {
	if err := addLimitRangeQuantityToResourceList(&lrItem.Default, lim.Default); err != nil {
		return err
	}
	return addLimitRangeQuantityToResourceList(&lrItem.DefaultRequest, lim.DefaultRequest)
}

func newLimitRange(a *app.App) (*k8sv1.LimitRange, error) {
	lrItem := k8sv1.LimitRangeItem{Type: k8sv1.LimitTypeContainer}
	if err := parseLimitRangeParams(&lrItem, a.Limits); err != nil {
		return nil, err
	}

	lr := &k8sv1.LimitRange{
		ObjectMeta: k8sv1.ObjectMeta{
			Name: "limits",
		},
		Spec: k8sv1.LimitRangeSpec{
			Limits: []k8sv1.LimitRangeItem{lrItem},
		},
	}
	return lr, nil
}

func newHPA(a *app.App) *asv1.HorizontalPodAutoscaler {
	tcpu := a.AutoScale.CPUTargetUtilization
	minr := a.AutoScale.Min

	return &asv1.HorizontalPodAutoscaler{
		ObjectMeta: k8sv1.ObjectMeta{
			Name:      a.Name,
			Namespace: a.Name,
		},
		Spec: asv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: asv1.CrossVersionObjectReference{
				APIVersion: "extensions/v1beta1",
				Kind:       "Deployment",
				Name:       a.Name,
			},
			TargetCPUUtilizationPercentage: &tcpu,
			MaxReplicas:                    a.AutoScale.Max,
			MinReplicas:                    &minr,
		},
	}
}

func (k *k8sClient) CreateNamespace(a *app.App, user string) error {
	ns := newNs(a, user)
	if err := addAppToNs(a, ns); err != nil {
		return err
	}

	_, err := k.kc.CoreV1().Namespaces().Create(ns)
	return err
}

func (k *k8sClient) CreateQuota(a *app.App) error {
	lr, err := newLimitRange(a)
	if err != nil {
		return err
	}

	_, err = k.kc.CoreV1().LimitRanges(a.Name).Create(lr)
	return err
}

func (k *k8sClient) CreateSecret(appName, secretName string, data map[string][]byte) error {
	s := &k8sv1.Secret{
		Type: k8sv1.SecretTypeOpaque,
		ObjectMeta: k8sv1.ObjectMeta{
			Name:      secretName,
			Namespace: appName,
		},
		Data: data,
	}

	_, err := k.kc.CoreV1().Secrets(appName).Create(s)
	return err
}

func (k *k8sClient) CreateAutoScale(a *app.App) error {
	hpa := newHPA(a)

	_, err := k.kc.AutoscalingV1().HorizontalPodAutoscalers(a.Name).Create(hpa)
	return err
}

func (k *k8sClient) AddressList(namespace string) ([]*app.Address, error) {
	srvs, err := k.kc.CoreV1().Services(namespace).List(k8sv1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "get addr list failed")
	}

	addrs := []*app.Address{}
	for _, srv := range srvs.Items {
		for _, i := range srv.Status.LoadBalancer.Ingress {
			h := i.Hostname
			if h == "" {
				h = i.IP
			}
			addrs = append(addrs, &app.Address{Hostname: h})
		}
	}
	return addrs, nil
}

func (k *k8sClient) Status(namespace string) (*app.Status, error) {
	hpa, err := k.kc.AutoscalingV1().HorizontalPodAutoscalers(namespace).Get(namespace)
	if err != nil {
		return nil, errors.Wrap(err, "get status failed")
	}

	pods, err := k.PodList(namespace)
	if err != nil {
		return nil, errors.Wrap(err, "get status failed")
	}

	var cpu int32
	if hpa.Status.CurrentCPUUtilizationPercentage != nil {
		cpu = *hpa.Status.CurrentCPUUtilizationPercentage
	}

	stat := &app.Status{
		CPU:  cpu,
		Pods: pods,
	}
	return stat, nil
}

func (k *k8sClient) AutoScale(namespace string) (*app.AutoScale, error) {
	hpa, err := k.kc.AutoscalingV1().HorizontalPodAutoscalers(namespace).Get(namespace)
	if err != nil {
		return nil, errors.Wrap(err, "get autoscale failed")
	}

	var cpu, min int32
	if hpa.Spec.TargetCPUUtilizationPercentage != nil {
		cpu = *hpa.Spec.TargetCPUUtilizationPercentage
	}
	if hpa.Spec.MinReplicas != nil {
		min = *hpa.Spec.MinReplicas
	}

	as := &app.AutoScale{
		CPUTargetUtilization: cpu,
		Min:                  min,
		Max:                  hpa.Spec.MaxReplicas,
	}
	return as, nil
}

func (k *k8sClient) Limits(namespace, name string) (*app.Limits, error) {
	lr, err := k.kc.CoreV1().LimitRanges(namespace).Get(name)
	if err != nil {
		return nil, errors.Wrap(err, "get limits failed")
	}

	var def, defReq []*app.LimitRangeQuantity
	for _, item := range lr.Spec.Limits {
		for k, v := range item.Default {
			q := &app.LimitRangeQuantity{
				Resource: string(k),
				Quantity: v.String(),
			}
			def = append(def, q)
		}
		for k, v := range item.DefaultRequest {
			q := &app.LimitRangeQuantity{
				Resource: string(k),
				Quantity: v.String(),
			}
			defReq = append(defReq, q)
		}
	}

	lim := &app.Limits{
		Default:        def,
		DefaultRequest: defReq,
	}
	return lim, nil
}

func newInClusterK8sClient() (Client, error) {
	conf, err := restclient.InClusterConfig()
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(conf)
	if err != nil {
		return nil, err
	}
	return &k8sClient{kc}, nil
}

func newOutOfClusterK8sClient(conf *Config) (Client, error) {
	k8sConf := &restclient.Config{
		Host:     conf.Host,
		Username: conf.Username,
		Password: conf.Password,
		Insecure: conf.Insecure,
	}
	kc, err := kubernetes.NewForConfig(k8sConf)
	if err != nil {
		return nil, err
	}
	return &k8sClient{kc}, nil
}