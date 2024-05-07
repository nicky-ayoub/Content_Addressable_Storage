package main

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
	"github.com/kushagra-gupta01/Content_Addressable_Storage/p2p"
)

type FileServerOpts struct {
	EncKey						[]byte
	StorageRoot       string
	PathTransformFunc PathTransformFunc
	Transport         p2p.Transport
	BootstrapNodes		[]string
}

type FileServer struct {
	FileServerOpts
	store 		*Store
	quitCh 		chan struct{}
	peers			map[string]p2p.Peer
	peerLock 	sync.Mutex
}

func NewFileServer(opts FileServerOpts) *FileServer {
	storeOpts := StoreOpts{
		Root:              opts.StorageRoot,
		PathTransformFunc: opts.PathTransformFunc,
	}
	return &FileServer{
		FileServerOpts: opts,
		store:          NewStore(storeOpts),
		quitCh: make(chan struct{}),
		peers: make(map[string]p2p.Peer),
	}
}

type Message struct{
	Payload any
}

func (s *FileServer) broadcast(msg *Message) error{
	buf:= new(bytes.Buffer)
	if err:= gob.NewEncoder(buf).Encode(msg);err!=nil{
		return err
	}

	for _,peer :=range s.peers{
		peer.Send([]byte{p2p.IncomingMessage})
		if err:= peer.Send(buf.Bytes());err!=nil{
			return err
		}
	}
	return nil
}

type MessageStoreFile struct{
	Key string
	Size int64
}

type MessageGetFile struct{
	Key string
}

func (s *FileServer) Get(key string) (io.Reader,error){
	if s.store.Has(key){
		fmt.Printf("[%s] serving file (%s) from local disk\n", s.Transport.Addr(),key)
		_,r,err:=s.store.Read(key)
		return r,err
	}
	fmt.Printf("[%s] don't have the file (%s) locally, fetching from network...\n",s.Transport.Addr(),key)

	msg:= Message{
		MessageGetFile{
			Key: key,
		},
	}

	if err:= s.broadcast(&msg);err!=nil{
		return nil, err
	}
	time.Sleep(500*time.Millisecond)
	for _,peer := range s.peers{
		//First read the file size so we can limit the amount of bytes
		// that we read from connection, so it ll not keep hanging.
		var fileSize int64
		binary.Read(peer,binary.LittleEndian,&fileSize)
		n,err := s.store.WriteDecrypt(s.EncKey,key,io.LimitReader(peer,fileSize))
		if err!=nil{
		return nil,err
		}

		fmt.Printf("[%s] recieved (%d) bytes over the network from (%s)\n",s.Transport.Addr(),n,peer.RemoteAddr())

		peer.CloseStream()
	}
	_,r,err:=s.store.Read(key)
	return r,err
}

func (s *FileServer) Store(key string,r io.Reader) error{
	//1. Store this file to disk
	//2. broadcast this file to all known peers in the network
	var(
	fileBuffer = new(bytes.Buffer)
	tee = io.TeeReader(r,fileBuffer)
	)
	size,err:= s.store.Write(key,tee)
	if err!=nil{
		return err
	}
	msg:= Message{
		Payload: MessageStoreFile{
			Key: key,
			Size: size+16,
		},
	}
	if err:= s.broadcast(&msg);err!=nil{
		return err
	}

	time.Sleep(5*time.Millisecond)

	peers:= []io.Writer{}
	for _,peer := range s.peers{
		peers=append(peers, peer)
	}
	mw:= io.MultiWriter(peers...)
	mw.Write([]byte{p2p.IncomingStream})
	n,err:= copyEncrypt(s.EncKey,fileBuffer,mw)
	if err!=nil{
		return err
	}

		fmt.Printf("[%s] received and written (%d) bytes to disk\n",s.Transport.Addr(),n)
		return nil
	}

func (s *FileServer) Stop(){
	close(s.quitCh)
}

func (s *FileServer) OnPeer(p p2p.Peer)error{
	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	s.peers[p.RemoteAddr().String()] = p
	log.Printf("connected with remote %s",p.RemoteAddr())
	return nil
}

func (s *FileServer) loop(){
	defer func(){
		log.Println("file server stopped due to error or user quit action")
		s.Transport.Close()
	}()
	for{
		select{
		case rpc:= <-s.Transport.Consume():
			var msg Message
			if err:= gob.NewDecoder(bytes.NewReader(rpc.Payload)).Decode(&msg);err!=nil{
				log.Println("decoding error:",err)
			}

			if err:= s.handleMessage(rpc.From,&msg);err!=nil{
				log.Println("handle message error:",err)
			}
		case <-s.quitCh: 
			return
		}
	}
}

func(s *FileServer) handleMessage(from string,msg *Message)error{
	switch v := msg.Payload.(type){
	case MessageStoreFile:
		return s.handleMessageStoreFile(from,v)
	case MessageGetFile:
		return s.handleMessageGetFile(from,v)
	}
	return nil
}

func (s *FileServer) handleMessageGetFile(from string,msg MessageGetFile) error{
	if !s.store.Has(msg.Key) {
		return fmt.Errorf("[%s] need to serve file (%s) but it does not exists on disk",s.Transport.Addr(),msg.Key)
	}
	fmt.Printf("[%s] serving file (%s) over the network\n",s.Transport.Addr(),msg.Key)
	fileSize,r,err:= s.store.Read(msg.Key)
	if err !=nil{
		return err
	}

	if rc,ok:= r.(io.ReadCloser);ok{
		fmt.Printf("closing readCloser")
		defer rc.Close()
	}

	peer,ok := s.peers[from]
	if !ok{
		return fmt.Errorf("peer %s not in map",from)
	}

	//First send the "incommingStream" byte to the peer and then 
	//we can send the file size as an int64
	peer.Send([]byte{p2p.IncomingStream})
	binary.Write(peer,binary.LittleEndian,fileSize)
	n,err := io.Copy(peer,r)
	if err !=nil{
		return err
	}
	fmt.Printf("[%s] written (%d) bytes over the network to %s\n",s.Transport.Addr(),n,from)

	return nil
}

func (s *FileServer) handleMessageStoreFile(from string,msg MessageStoreFile) error{
	peer,ok:= s.peers[from]
	if !ok{
		return fmt.Errorf("peer (%s) could not be found in peerlist",from)
	}
	n,err:= s.store.Write(msg.Key,io.LimitReader(peer,msg.Size))
	if err!=nil{
		return err
	}
	fmt.Printf("[%s] written %d bytes to disk\n",s.Transport.Addr(),n)
	peer.CloseStream()
	// peer.(*p2p.TCPpeer).Wg.Done()
	return nil
} 	

func (s *FileServer) bootstrapNetwork() error{
	for _,addr := range s.BootstrapNodes{
		if len(addr)==0{continue}
		go func(addr string){
			fmt.Printf("[%s] attempting to connect with remote: %s\n",s.Transport.Addr(),addr)
			if err:= s.Transport.Dial(addr);err!=nil{
				log.Println("dial error:",err)
			}
		}(addr)
	}
	return nil
}

func (s *FileServer) Start() error{
	fmt.Printf("[%s] starting fileserver...\n",s.Transport.Addr())
	if err:= s.Transport.ListenAndAccept();err!=nil{
		return err
	}

	s.bootstrapNetwork()
	s.loop()
	return  nil
}

func init(){
	gob.Register(MessageStoreFile{})
	gob.Register(MessageGetFile{})
}