default:
	go install -v ./go/datastore_backup

push: default
	./build_release "`git log -n1 --format=%s`"
	go install -v ../push/go/pushcli
	pushcli datastore-backup skia-monitoring

.PHONY: default push
