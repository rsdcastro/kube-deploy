FROM alpine:3.6

RUN apk update && apk add openssh-client bash
RUN echo "StrictHostKeyChecking no" >> /etc/ssh/ssh_config

RUN echo "UserKnownHostsFile /dev/null" >> /etc/ssh/ssh_config
ADD machine-controller /bin
ADD bin/terraform_0.11.3_linux_amd64 /bin/terraform
ADD templates/gcp-instance.tf /bin/gcp-instance.tf

ENTRYPOINT ["/bin/machine-controller"]
