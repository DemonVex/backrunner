FROM ubuntu:trusty

RUN apt-get update && apt-get -y upgrade && \
	apt-get install -y curl git g++ make && \
	curl http://repo.reverbrain.com/REVERBRAIN.GPG | apt-key add - && \
	echo "deb http://repo.reverbrain.com/trusty/ current/amd64/" > /etc/apt/sources.list.d/reverbrain.list && \
	echo "deb http://repo.reverbrain.com/trusty/ current/all/" >> /etc/apt/sources.list.d/reverbrain.list && \
	apt-get update && \
	apt-get install -y elliptics-client elliptics-dev && \
	rm -rf /var/lib/apt/lists/*

RUN export PATH=$PATH:/usr/local/go/bin:/root/go/bin && \
	export GOPATH=/root/go && \
	VERSION=go1.5.2 && \
	curl -f -I https://storage.googleapis.com/golang/$VERSION.linux-amd64.tar.gz && \
	test `go version | awk {'print $3'}` = $VERSION || \
	echo "Downloading" && \
	curl -O https://storage.googleapis.com/golang/$VERSION.linux-amd64.tar.gz && \
	rm -rf /usr/local/go && \
	tar -C /usr/local -xf $VERSION.linux-amd64.tar.gz && \
	rm -f $VERSION.linux-amd64.tar.gz
	
RUN export PATH=$PATH:/usr/local/go/bin:/root/go/bin && \
	export GOPATH=/root/go && \
	mkdir -p /root/go/src/github.com/bioothod && \
	cd /root/go/src/github.com/bioothod && \
	git clone https://github.com/bioothod/elliptics-go.git && \
	cd /root/go/src/github.com/bioothod/elliptics-go/elliptics && \
	go install && \
	echo "Go binding has been updated" && \
	mkdir -p /root/go/src/github.com/DemonVex && \
	cd /root/go/src/github.com/DemonVex && \
	git clone https://github.com/DemonVex/backrunner.git && \
	cd /root/go/src/github.com/DemonVex/backrunner && \
	go get && go install && \
	echo "Backrunner has been updated";

EXPOSE 9090 8080 443